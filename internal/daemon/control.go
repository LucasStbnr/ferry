package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LucasStbnr/ferry/internal/control"
	"github.com/LucasStbnr/ferry/internal/mailsync"
	"github.com/LucasStbnr/ferry/internal/store"
)

// The daemon answers the CLI over the control socket. These methods run on a
// caller's goroutine, so they only read shared state under the lock and do the
// real work through the same syncers the background loops use.

var _ control.Handler = (*Daemon)(nil)

// Status reports the daemon's state.
func (d *Daemon) Status(ctx context.Context) (*control.Status, error) {
	statuses, err := d.opts.Manager.Status(ctx)
	if err != nil {
		return nil, err
	}

	enabled := map[string]bool{}
	if d.hooks != nil {
		for _, name := range d.hooks.Accounts() {
			enabled[name] = true
		}
	}

	out := &control.Status{
		Version:   d.opts.Version,
		StartedAt: d.startedAt,
		Uptime:    time.Since(d.startedAt).Round(time.Second).String(),
		Accounts:  make([]control.AccountStatus, 0, len(statuses)),
	}
	if d.imap != nil {
		out.IMAPAddr = d.opts.Config.IMAP.Addr
	}
	if d.smtp != nil {
		out.SMTPAddr = d.opts.Config.SMTP.Addr
	}
	if d.hookSrv != nil {
		out.Webhook = d.hookSrv.Addr + d.opts.Config.Webhook.Path
	}

	for _, s := range statuses {
		lastSync := s.Received.LastSyncAt
		if s.Sent.LastSyncAt.After(lastSync) {
			lastSync = s.Sent.LastSyncAt
		}
		lastErr := s.Received.LastError
		if lastErr == "" {
			lastErr = s.Sent.LastError
		}
		out.Accounts = append(out.Accounts, control.AccountStatus{
			Name:            s.Name,
			Address:         s.Address,
			Domains:         s.Domains,
			Messages:        s.Counts.Messages,
			Unseen:          s.Counts.Unseen,
			Bytes:           s.Counts.Bytes,
			Tombstones:      s.Counts.Tombstones,
			LastSync:        lastSync,
			BackfillDone:    s.Received.BackfillDone && s.Sent.BackfillDone,
			LastError:       lastErr,
			WebhooksEnabled: enabled[s.Name],
		})
	}
	return out, nil
}

// Sync runs a sync pass now, rather than waiting for the next tick.
func (d *Daemon) Sync(ctx context.Context, req control.SyncRequest) (*control.SyncResult, error) {
	targets, err := d.targets(req.Account)
	if err != nil {
		return nil, err
	}

	res := &control.SyncResult{}
	for _, name := range sortedKeys(targets) {
		syncer := targets[name]
		if req.Backfill {
			if err := syncer.ResetBackfill(ctx); err != nil {
				return nil, fmt.Errorf("daemon: reset backfill for %s: %w", name, err)
			}
		}

		one := control.AccountSyncResult{Account: name}
		// A forced backfill walks the whole history, so keep going until it
		// finishes rather than returning after a single page.
		for {
			r, err := syncer.Sync(ctx)
			one.Received += r.ReceivedStored
			one.Sent += r.SentStored
			for _, e := range r.Errors {
				one.Errors = append(one.Errors, e.Error())
			}
			if err != nil {
				one.Errors = append(one.Errors, err.Error())
				break
			}
			if !r.BackfillPending {
				break
			}
			if !req.Backfill {
				// An ordinary sync reports that history is still catching up
				// and lets the background loop continue it.
				one.BackfillPending = true
				break
			}
			if ctx.Err() != nil {
				one.BackfillPending = true
				break
			}
		}
		res.Accounts = append(res.Accounts, one)
	}
	return res, nil
}

// Reload picks up accounts added or removed while the daemon was running.
func (d *Daemon) Reload(ctx context.Context) error {
	accounts, err := d.opts.DB.Accounts(ctx)
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(accounts))
	for i := range accounts {
		live[accounts[i].Name] = true
	}

	d.mu.Lock()
	var gone []string
	for name := range d.syncers {
		if !live[name] {
			gone = append(gone, name)
		}
	}
	for _, name := range gone {
		if cancel, ok := d.cancels[name]; ok {
			cancel()
		}
		delete(d.syncers, name)
		delete(d.cancels, name)
	}
	known := make(map[string]bool, len(d.syncers))
	for name := range d.syncers {
		known[name] = true
	}
	d.mu.Unlock()

	for _, name := range gone {
		if d.imap != nil {
			d.imap.ForgetAccount(name)
		}
		if d.hooks != nil {
			d.hooks.Unregister(name)
		}
		d.log.Info("account removed", "account", name)
	}
	for i := range accounts {
		if known[accounts[i].Name] {
			continue
		}
		d.log.Info("account added", "account", accounts[i].Name)
		d.startAccount(context.WithoutCancel(ctx), &accounts[i])
	}
	return nil
}

// targets resolves an account name, or every account when it is empty.
func (d *Daemon) targets(name string) (map[string]*mailsync.Syncer, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if name == "" {
		out := make(map[string]*mailsync.Syncer, len(d.syncers))
		for k, v := range d.syncers {
			out[k] = v
		}
		if len(out) == 0 {
			return nil, errors.New("daemon: no accounts are syncing; check that each one has an API key")
		}
		return out, nil
	}

	syncer, ok := d.syncers[name]
	if !ok {
		return nil, fmt.Errorf("%w: account %q is not syncing", store.ErrNotFound, name)
	}
	return map[string]*mailsync.Syncer{name: syncer}, nil
}

func sortedKeys(m map[string]*mailsync.Syncer) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
