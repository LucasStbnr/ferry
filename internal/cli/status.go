package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/LucasStbnr/ferry/internal/control"
)

func newStatusCmd(e *env) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the daemon and account status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := e.loadConfig(); err != nil {
				return err
			}

			var status *control.Status
			running := control.Available(ctx, e.cfg.ControlSocket())
			if running {
				var err error
				if status, err = control.Dial(e.cfg.ControlSocket()).Status(ctx); err != nil {
					return err
				}
			} else {
				var err error
				if status, err = e.statusOffline(ctx); err != nil {
					return err
				}
			}

			if asJSON {
				enc := json.NewEncoder(e.out)
				enc.SetIndent("", "  ")
				return enc.Encode(status)
			}
			printStatus(e, status, running)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return cmd
}

// statusOffline builds the same report by reading the database, so `ferry
// status` says something useful when the daemon is not running.
func (e *env) statusOffline(ctx context.Context) (*control.Status, error) {
	if err := e.open(ctx); err != nil {
		return nil, err
	}
	defer e.close()

	statuses, err := e.mgr.Status(ctx)
	if err != nil {
		return nil, err
	}
	out := &control.Status{Version: version}
	for _, a := range statuses {
		lastSync := a.Received.LastSyncAt
		if a.Sent.LastSyncAt.After(lastSync) {
			lastSync = a.Sent.LastSyncAt
		}
		lastErr := a.Received.LastError
		if lastErr == "" {
			lastErr = a.Sent.LastError
		}
		out.Accounts = append(out.Accounts, control.AccountStatus{
			Name:            a.Name,
			Address:         a.Address,
			Domains:         a.Domains,
			Messages:        a.Counts.Messages,
			Unseen:          a.Counts.Unseen,
			Bytes:           a.Counts.Bytes,
			Tombstones:      a.Counts.Tombstones,
			LastSync:        lastSync,
			BackfillDone:    a.Received.BackfillDone && a.Sent.BackfillDone,
			LastError:       lastErr,
			WebhooksEnabled: a.HasWebhook,
		})
	}
	return out, nil
}

func printStatus(e *env, s *control.Status, running bool) {
	if running {
		e.printf("Daemon     running, up %s (ferry %s)\n", s.Uptime, s.Version)
		e.printf("IMAP       %s\n", orNone(s.IMAPAddr))
		e.printf("SMTP       %s\n", orNone(s.SMTPAddr))
		if s.Webhook != "" {
			e.printf("Webhooks   %s\n", s.Webhook)
		}
	} else {
		e.printf("Daemon     not running (start it with `ferry serve`, or `brew services start ferry`)\n")
	}
	e.printf("Data       %s\n\n", e.cfg.Dir)

	if len(s.Accounts) == 0 {
		e.printf("No accounts yet. Add one with `ferry account add <name>`.\n")
		return
	}

	w := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ACCOUNT\tMESSAGES\tUNREAD\tSIZE\tLAST SYNC\tHISTORY\tWEBHOOKS")
	for _, a := range s.Accounts {
		history := "downloading"
		if a.BackfillDone {
			history = "complete"
		}
		hooks := "off"
		if a.WebhooksEnabled {
			hooks = "on"
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
			a.Name, a.Messages, a.Unseen, humanBytes(a.Bytes),
			relativeTime(a.LastSync), history, hooks)
	}
	_ = w.Flush()

	for _, a := range s.Accounts {
		if a.LastError != "" {
			e.printf("\n%s last sync error: %s\n", a.Name, a.LastError)
		}
		if len(a.Domains) == 0 {
			e.printf("\n%s has no verified sending domain; sending is refused. "+
				"Verify one in Resend, then run `ferry account refresh %s`.\n", a.Name, a.Name)
		}
		if a.Tombstones > 0 {
			e.printf("\n%s: %d message(s) deleted locally and held back from re-syncing.\n", a.Name, a.Tombstones)
		}
	}
}

func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Format("2006-01-02 15:04")
}
