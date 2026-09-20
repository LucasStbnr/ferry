package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/LucasStbnr/ferry/internal/config"
	"github.com/LucasStbnr/ferry/internal/daemon"
	"github.com/LucasStbnr/ferry/internal/tlsutil"
)

func newServeCmd(e *env) *cobra.Command {
	var (
		imapAddr string
		smtpAddr string
		hookAddr string
		noSync   bool
		trace    bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Ferry daemon",
		Long: `Runs the IMAP and SMTP servers, the sync loop for every account and, when
configured, the webhook receiver.

Both mail servers use implicit TLS from the first byte, and both bind to
loopback by default, so an app password never crosses a network. Serving on
0.0.0.0 is only sensible when self-hosting behind a real certificate; set the
addresses in config.json to do that deliberately.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := e.open(ctx); err != nil {
				return err
			}
			defer e.close()

			if imapAddr != "" {
				e.cfg.IMAP.Addr = imapAddr
			}
			if smtpAddr != "" {
				e.cfg.SMTP.Addr = smtpAddr
			}
			if hookAddr != "" {
				e.cfg.Webhook.Addr = hookAddr
			}
			if noSync {
				e.cfg.Sync.Interval = 0
			}

			bundle, err := loadTLS(e.cfg)
			if err != nil {
				return err
			}
			warnIfExposed(e)

			// Kept as io.Writer rather than *os.File: a nil *os.File stored in
			// an interface is not a nil interface, and the servers would then
			// write to it and panic.
			var traceWriter io.Writer
			if trace {
				path := e.cfg.Path("protocol.log")
				f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					return fmt.Errorf("open protocol trace: %w", err)
				}
				defer func() { _ = f.Close() }()
				traceWriter = f
				e.log.Warn("protocol tracing is on; the trace contains app passwords in plain text",
					"path", path)
			}

			d, err := daemon.New(daemon.Options{
				Trace:   traceWriter,
				Config:  e.cfg,
				DB:      e.db,
				Secrets: e.sec,
				Manager: e.mgr,
				Bundle:  bundle,
				Logger:  e.log,
				Version: version,
			})
			if err != nil {
				return err
			}

			e.log.Info("ferry starting", "version", version, "data_dir", e.cfg.Dir)
			return d.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&imapAddr, "imap-addr", "", "override the IMAP listen address")
	cmd.Flags().StringVar(&smtpAddr, "smtp-addr", "", "override the SMTP listen address")
	cmd.Flags().StringVar(&hookAddr, "webhook-addr", "", "enable the webhook receiver on this address")
	cmd.Flags().BoolVar(&noSync, "no-poll", false, "do not poll Resend; sync only on demand or from webhooks")
	cmd.Flags().BoolVar(&trace, "trace-protocol", false,
		"log the raw IMAP and SMTP conversation to protocol.log (contains app passwords; for debugging only)")
	return cmd
}

// loadTLS uses the operator's certificate when one is configured, and
// otherwise generates and maintains Ferry's own local CA and leaf.
func loadTLS(cfg config.Config) (*tlsutil.Bundle, error) {
	if cfg.TLS.CertFile != "" {
		return tlsutil.LoadBundle(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	}
	return tlsutil.EnsureBundle(cfg.Path("tls"), cfg.TLS.Hostnames)
}

// warnIfExposed says something when Ferry is about to listen beyond loopback
// with a certificate no outside client will trust. That combination is almost
// always a mistake, and silently accepting it would put an app password on the
// network behind a certificate clients are being told to ignore.
func warnIfExposed(e *env) {
	if e.cfg.TLS.CertFile != "" {
		return
	}
	for name, addr := range map[string]string{
		"IMAP":    e.cfg.IMAP.Addr,
		"SMTP":    e.cfg.SMTP.Addr,
		"webhook": e.cfg.Webhook.Addr,
	} {
		if addr == "" || config.IsLoopback(addr) {
			continue
		}
		e.log.Warn(fmt.Sprintf(
			"%s is listening beyond loopback with a locally generated certificate; "+
				"set tls.cert_file and tls.key_file in config.json to serve a certificate clients can verify",
			name), "addr", addr)
	}
}
