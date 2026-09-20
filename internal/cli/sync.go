package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/LucasStbnr/ferry/internal/control"
	"github.com/LucasStbnr/ferry/internal/mailsync"
	"github.com/LucasStbnr/ferry/internal/resend"
	"github.com/LucasStbnr/ferry/internal/store"
)

func newSyncCmd(e *env) *cobra.Command {
	var backfill bool
	cmd := &cobra.Command{
		Use:   "sync [account]",
		Short: "Fetch mail from Resend now",
		Long: `Runs a sync immediately instead of waiting for the next poll.

If the daemon is running, the work happens there, because only one process may
write to the database. Otherwise this command does it directly, which is what
makes a one-off fetch possible without starting the service.

With --backfill the history cursors are reset and the whole archive is walked
again. Messages already stored are recognised and skipped, and messages
deleted locally stay deleted, so a re-walk costs API calls but never produces
duplicates.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := e.loadConfig(); err != nil {
				return err
			}
			var accountName string
			if len(args) == 1 {
				accountName = args[0]
			}

			req := control.SyncRequest{Account: accountName, Backfill: backfill}
			if control.Available(ctx, e.cfg.ControlSocket()) {
				res, err := control.Dial(e.cfg.ControlSocket()).Sync(ctx, req)
				if err != nil {
					return err
				}
				printSyncResult(e, res)
				return nil
			}

			res, err := e.syncDirectly(ctx, req)
			if err != nil {
				return err
			}
			printSyncResult(e, res)
			return nil
		},
	}
	cmd.Flags().BoolVar(&backfill, "backfill", false, "reset the history cursors and walk the whole archive again")
	return cmd
}

// syncDirectly runs the sync in this process, for when no daemon is running.
func (e *env) syncDirectly(ctx context.Context, req control.SyncRequest) (*control.SyncResult, error) {
	if err := e.open(ctx); err != nil {
		return nil, err
	}
	defer e.close()

	accounts, err := e.db.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, errors.New("no accounts configured; add one with `ferry account add <name>`")
	}

	res := &control.SyncResult{}
	found := false
	for i := range accounts {
		acct := &accounts[i]
		if req.Account != "" && acct.Name != req.Account {
			continue
		}
		found = true

		apiKey, err := e.mgr.APIKey(acct.Name)
		if err != nil {
			return nil, err
		}
		client := resend.New(apiKey,
			resend.WithRate(e.cfg.Sync.RequestsPerSecond),
			resend.WithUserAgent("ferry/"+version))

		syncer := mailsync.New(client, e.db.Account(acct), mailsync.Options{
			PageSize:        e.cfg.Sync.BackfillPageSize,
			MaxMessageBytes: e.cfg.Sync.MaxMessageBytes,
			Logger:          e.log,
		})
		if req.Backfill {
			if err := syncer.ResetBackfill(ctx); err != nil {
				return nil, err
			}
		}

		one := control.AccountSyncResult{Account: acct.Name}
		start := time.Now()
		for {
			r, err := syncer.Sync(ctx)
			one.Received += r.ReceivedStored
			one.Sent += r.SentStored
			for _, se := range r.Errors {
				one.Errors = append(one.Errors, se.Error())
			}
			if err != nil {
				one.Errors = append(one.Errors, err.Error())
				break
			}
			if !r.BackfillPending {
				break
			}
			if ctx.Err() != nil {
				one.BackfillPending = true
				break
			}
			// A first run downloads the whole history; say so rather than
			// appearing to hang.
			if time.Since(start) > 5*time.Second {
				e.printf("  %s: %d messages so far…\n", acct.Name, one.Received+one.Sent)
				start = time.Now()
			}
		}
		res.Accounts = append(res.Accounts, one)
	}

	if !found {
		return nil, fmt.Errorf("%w: account %q", store.ErrNotFound, req.Account)
	}
	return res, nil
}

func printSyncResult(e *env, res *control.SyncResult) {
	total := 0
	for _, a := range res.Accounts {
		total += a.Received + a.Sent
		switch {
		case a.Received == 0 && a.Sent == 0:
			e.printf("%s: up to date\n", a.Account)
		default:
			e.printf("%s: %d received, %d sent\n", a.Account, a.Received, a.Sent)
		}
		if a.BackfillPending {
			e.printf("  history is still downloading; run `ferry sync --backfill` to finish it now\n")
		}
		for _, msg := range a.Errors {
			e.printf("  warning: %s\n", msg)
		}
	}
	if total == 0 && len(res.Accounts) == 0 {
		e.printf("Nothing to sync.\n")
	}
}
