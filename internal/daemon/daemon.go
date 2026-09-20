// Package daemon assembles the running service: the IMAP and SMTP servers, one
// sync loop per account, the optional webhook receiver and the control socket.
//
// It owns the lifecycle. Everything starts together, a failure in any component
// stops the rest, and shutdown waits for in-flight work so a send in progress is
// never abandoned halfway.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/LucasStbnr/ferry/internal/account"
	"github.com/LucasStbnr/ferry/internal/config"
	"github.com/LucasStbnr/ferry/internal/control"
	"github.com/LucasStbnr/ferry/internal/imapd"
	"github.com/LucasStbnr/ferry/internal/mailsync"
	"github.com/LucasStbnr/ferry/internal/resend"
	"github.com/LucasStbnr/ferry/internal/secrets"
	"github.com/LucasStbnr/ferry/internal/smtpd"
	"github.com/LucasStbnr/ferry/internal/store"
	"github.com/LucasStbnr/ferry/internal/tlsutil"
	"github.com/LucasStbnr/ferry/internal/webhook"
)

// Options configure the daemon.
type Options struct {
	Config  config.Config
	DB      *store.DB
	Secrets secrets.Store
	Manager *account.Manager
	Bundle  *tlsutil.Bundle
	Logger  *slog.Logger
	// Version is reported by `ferry status`.
	Version string
}

// Daemon is the running service.
type Daemon struct {
	opts      Options
	log       *slog.Logger
	startedAt time.Time

	imap    *imapd.Server
	smtp    *smtpd.Server
	hooks   *webhook.Receiver
	hookSrv *http.Server
	ctl     *control.Server

	mu      sync.RWMutex
	syncers map[string]*mailsync.Syncer
	cancels map[string]context.CancelFunc

	group sync.WaitGroup
}

// New builds a daemon. Listeners are bound in Run, so a configuration error is
// reported before anything is exposed.
func New(opts Options) (*Daemon, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	d := &Daemon{
		opts:      opts,
		log:       opts.Logger,
		startedAt: time.Now(),
		syncers:   map[string]*mailsync.Syncer{},
		cancels:   map[string]context.CancelFunc{},
	}

	serverTLS, err := opts.Bundle.ServerConfig()
	if err != nil {
		return nil, err
	}

	if !opts.Config.IMAP.Disabled {
		d.imap, err = imapd.New(imapd.Options{
			Addr:        opts.Config.IMAP.Addr,
			TLSConfig:   serverTLS,
			DB:          opts.DB,
			Auth:        opts.Manager,
			AppendLimit: opts.Config.Sync.MaxMessageBytes,
			Logger:      opts.Logger,
		})
		if err != nil {
			return nil, err
		}
	}

	if !opts.Config.SMTP.Disabled {
		d.smtp, err = smtpd.New(smtpd.Options{
			Addr:            opts.Config.SMTP.Addr,
			TLSConfig:       serverTLS,
			DB:              opts.DB,
			Auth:            opts.Manager,
			Clients:         opts.Manager,
			MaxMessageBytes: opts.Config.Sync.MaxMessageBytes,
			Logger:          opts.Logger,
			OnSent:          d.notifyMailbox,
		})
		if err != nil {
			return nil, err
		}
	}

	if opts.Config.Webhook.Addr != "" {
		d.hooks = webhook.New(webhook.Options{
			Path:     opts.Config.Webhook.Path,
			DB:       opts.DB,
			Logger:   opts.Logger,
			Notifier: d.notifyMailbox,
		})
		d.hookSrv = &http.Server{
			Addr:              opts.Config.Webhook.Addr,
			Handler:           d.hooks.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		if opts.Config.Webhook.TLS {
			d.hookSrv.TLSConfig = serverTLS
		}
	}

	d.ctl = control.NewServer(opts.Config.ControlSocket(), d, opts.Logger)
	return d, nil
}

// notifyMailbox tells the IMAP server a mailbox changed, so clients idling on
// it hear about new mail without waiting for their next poll.
func (d *Daemon) notifyMailbox(acctName string, mailboxID int64) {
	if d.imap != nil {
		d.imap.MailboxChanged(acctName, mailboxID)
	}
}

// MailboxChanged implements mailsync.Notifier.
func (d *Daemon) MailboxChanged(acctName string, mailboxID int64) {
	d.notifyMailbox(acctName, mailboxID)
}

// Run starts every component and blocks until ctx is cancelled or a component
// fails. It always shuts the others down before returning.
func (d *Daemon) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, 4)

	// Listeners are bound first and all together: a daemon that came up with
	// IMAP but no SMTP would look healthy while silently failing to send.
	var listeners []net.Listener
	closeAll := func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}

	if d.imap != nil {
		ln, err := d.imap.Listen(ctx)
		if err != nil {
			closeAll()
			return err
		}
		listeners = append(listeners, ln)
		d.log.Info("imap listening", "addr", d.opts.Config.IMAP.Addr,
			"loopback_only", config.IsLoopback(d.opts.Config.IMAP.Addr))
		d.group.Add(1)
		go func() {
			defer d.group.Done()
			errs <- ignoreClosed(d.imap.Serve(ln))
		}()
	}

	if d.smtp != nil {
		ln, err := d.smtp.Listen(ctx)
		if err != nil {
			closeAll()
			return err
		}
		listeners = append(listeners, ln)
		d.log.Info("smtp listening", "addr", d.opts.Config.SMTP.Addr,
			"loopback_only", config.IsLoopback(d.opts.Config.SMTP.Addr))
		d.group.Add(1)
		go func() {
			defer d.group.Done()
			errs <- ignoreClosed(d.smtp.Serve(ln))
		}()
	}

	if err := d.ctl.Listen(ctx); err != nil {
		closeAll()
		return err
	}
	d.group.Add(1)
	go func() {
		defer d.group.Done()
		errs <- ignoreClosed(d.ctl.Serve())
	}()

	if d.hookSrv != nil {
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", d.hookSrv.Addr)
		if err != nil {
			closeAll()
			_ = d.ctl.Close()
			return fmt.Errorf("daemon: webhook listen on %s: %w", d.hookSrv.Addr, err)
		}
		listeners = append(listeners, ln)
		d.log.Info("webhook receiver listening", "addr", d.hookSrv.Addr,
			"path", d.opts.Config.Webhook.Path, "tls", d.opts.Config.Webhook.TLS)
		d.group.Add(1)
		go func() {
			defer d.group.Done()
			if d.hookSrv.TLSConfig != nil {
				errs <- ignoreClosed(d.hookSrv.ServeTLS(ln, "", ""))
				return
			}
			errs <- ignoreClosed(d.hookSrv.Serve(ln))
		}()
	}

	if err := d.startAccounts(ctx); err != nil {
		closeAll()
		_ = d.ctl.Close()
		return err
	}

	var runErr error
	select {
	case <-ctx.Done():
		d.log.Info("shutting down")
	case runErr = <-errs:
		if runErr != nil {
			d.log.Error("component failed", "error", runErr)
		}
	}

	cancel()
	d.shutdown()
	closeAll()
	d.group.Wait()
	return runErr
}

// shutdown stops the components. It deliberately starts a fresh context
// rather than inheriting the daemon's: the caller's context is already
// cancelled by the time we get here, and in-flight work needs a bounded grace
// period, not immediate cancellation.
//
//nolint:contextcheck // the detached context is the point
func (d *Daemon) shutdown() {
	// Give in-flight work a bounded chance to finish: an SMTP session waiting
	// on Resend should not be cut off mid-send.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if d.hookSrv != nil {
		_ = d.hookSrv.Shutdown(shutdownCtx)
	}
	if d.smtp != nil {
		_ = d.smtp.Close()
	}
	if d.imap != nil {
		_ = d.imap.Close()
	}
	_ = d.ctl.Close()
}

// startAccounts creates a sync loop for every account that has an API key.
func (d *Daemon) startAccounts(ctx context.Context) error {
	accounts, err := d.opts.DB.Accounts(ctx)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		d.log.Warn("no accounts configured; run `ferry account add` to add one")
	}
	for i := range accounts {
		d.startAccount(ctx, &accounts[i])
	}
	return nil
}

func (d *Daemon) startAccount(ctx context.Context, acct *store.Account) {
	log := d.log.With("account", acct.Name)

	apiKey, err := d.opts.Manager.APIKey(acct.Name)
	if err != nil {
		// Without a key the account can still be read in Mail; it simply does
		// not sync. Failing the whole daemon would take the other accounts
		// down with it.
		log.Error("account will not sync", "error", err)
		return
	}

	client := resend.New(apiKey,
		resend.WithRate(d.opts.Config.Sync.RequestsPerSecond),
		resend.WithUserAgent("ferry/"+d.opts.Version))

	as := d.opts.DB.Account(acct)
	syncer := mailsync.New(client, as, mailsync.Options{
		PageSize:        d.opts.Config.Sync.BackfillPageSize,
		MaxMessageBytes: d.opts.Config.Sync.MaxMessageBytes,
		Interval:        d.opts.Config.Sync.Interval.D(),
		Logger:          d.opts.Logger,
		Notifier:        d,
	})

	if d.hooks != nil {
		secret, err := d.opts.Secrets.Get(secrets.WebhookSecret(acct.Name))
		switch {
		case err == nil:
			if err := d.hooks.Register(acct.Name, secret, syncer); err != nil {
				log.Error("webhooks disabled for this account", "error", err)
			} else {
				log.Info("webhooks enabled")
			}
		case errors.Is(err, secrets.ErrNotFound):
			log.Info("no webhook signing secret; this account syncs by polling only")
		default:
			log.Error("could not read webhook secret", "error", err)
		}
	}

	accountCtx, cancel := context.WithCancel(ctx)

	d.mu.Lock()
	if old, ok := d.cancels[acct.Name]; ok {
		old()
	}
	d.syncers[acct.Name] = syncer
	d.cancels[acct.Name] = cancel
	d.mu.Unlock()

	d.group.Add(1)
	go func() {
		defer d.group.Done()
		if err := syncer.Run(accountCtx); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Error("sync loop stopped", "error", err)
		}
	}()
}

func ignoreClosed(err error) error {
	switch {
	case err == nil,
		errors.Is(err, net.ErrClosed),
		errors.Is(err, http.ErrServerClosed),
		errors.Is(err, context.Canceled):
		return nil
	}
	return err
}
