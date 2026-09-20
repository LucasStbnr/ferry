package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/LucasStbnr/ferry/internal/control"
	"github.com/LucasStbnr/ferry/internal/secrets"
)

// check is one diagnostic and its outcome.
type check struct {
	name   string
	status status
	detail string
	fix    string
}

type status int

const (
	statusOK status = iota
	statusWarn
	statusFail
)

func (s status) symbol() string {
	switch s {
	case statusOK:
		return "ok  "
	case statusWarn:
		return "warn"
	}
	return "fail"
}

func newDoctorCmd(e *env) *cobra.Command {
	var repair bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the installation and report anything wrong",
		Long: `Runs every check Ferry can make locally: the data directory, the database,
the certificate, each account's credentials and whether the servers answer.

With --repair it also runs the safe repairs: a database integrity check, a
vacuum, and removal of stored message files that no longer belong to any
message.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := e.open(ctx); err != nil {
				return err
			}
			defer e.close()

			checks := e.runChecks(ctx, repair)

			worst := statusOK
			for _, c := range checks {
				e.printf("[%s] %s", c.status.symbol(), c.name)
				if c.detail != "" {
					e.printf(": %s", c.detail)
				}
				e.printf("\n")
				if c.fix != "" && c.status != statusOK {
					e.printf("       → %s\n", c.fix)
				}
				if c.status > worst {
					worst = c.status
				}
			}

			switch worst {
			case statusOK:
				e.printf("\nEverything looks healthy.\n")
				return nil
			case statusWarn:
				e.printf("\nFerry works, but some things are worth attention.\n")
				return nil
			}
			return errors.New("doctor found problems that need fixing")
		},
	}
	cmd.Flags().BoolVar(&repair, "repair", false, "run the safe repairs as well as the checks")
	return cmd
}

func (e *env) runChecks(ctx context.Context, repair bool) []check {
	// Six fixed checks plus two per account, which is the common shape.
	checks := make([]check, 0, 8)

	checks = append(checks, e.checkDataDir())
	checks = append(checks, e.checkDatabase(ctx, repair))
	checks = append(checks, e.checkTLS())
	checks = append(checks, e.checkSecretStore())
	checks = append(checks, e.checkAccounts(ctx, repair)...)
	checks = append(checks, e.checkDaemon(ctx))
	checks = append(checks, e.checkListeners(ctx)...)
	return checks
}

func (e *env) checkDataDir() check {
	c := check{name: "Data directory", detail: e.cfg.Dir}
	info, err := os.Stat(e.cfg.Dir)
	if err != nil {
		c.status = statusFail
		c.detail = err.Error()
		return c
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		c.status = statusWarn
		c.detail = fmt.Sprintf("%s is mode %04o; mail is readable by other users on this machine", e.cfg.Dir, perm)
		c.fix = fmt.Sprintf("chmod 700 %s", e.cfg.Dir)
	}
	return c
}

func (e *env) checkDatabase(ctx context.Context, repair bool) check {
	c := check{name: "Database"}
	if err := e.db.Check(ctx); err != nil {
		c.status = statusFail
		c.detail = err.Error()
		c.fix = "restore from a backup; a corrupt database cannot be repaired in place"
		return c
	}
	c.detail = e.cfg.DBPath()
	if repair {
		if err := e.db.Vacuum(ctx); err != nil {
			c.status = statusWarn
			c.detail = "integrity ok, but vacuum failed: " + err.Error()
			return c
		}
		c.detail += " (checked and vacuumed)"
	}
	return c
}

func (e *env) checkTLS() check {
	c := check{name: "TLS certificate"}
	bundle, err := loadTLS(e.cfg)
	if err != nil {
		c.status = statusFail
		c.detail = err.Error()
		c.fix = "run `ferry serve` once to generate a certificate, or set tls.cert_file in config.json"
		return c
	}
	notAfter, err := bundle.LeafNotAfter()
	if err != nil {
		c.status = statusFail
		c.detail = err.Error()
		return c
	}
	left := time.Until(notAfter)
	switch {
	case left <= 0:
		c.status = statusFail
		c.detail = "the certificate expired on " + notAfter.Format("2006-01-02")
		c.fix = "restart the daemon; Ferry issues a new certificate automatically"
	case left < 30*24*time.Hour:
		c.status = statusWarn
		c.detail = fmt.Sprintf("expires in %d days", int(left.Hours()/24))
		c.fix = "restart the daemon to renew it"
	default:
		c.detail = fmt.Sprintf("valid until %s", notAfter.Format("2006-01-02"))
	}
	return c
}

func (e *env) checkSecretStore() check {
	return check{name: "Secret store", detail: e.sec.Describe()}
}

func (e *env) checkAccounts(ctx context.Context, repair bool) []check {
	accounts, err := e.db.Accounts(ctx)
	if err != nil {
		return []check{{name: "Accounts", status: statusFail, detail: err.Error()}}
	}
	if len(accounts) == 0 {
		return []check{{
			name: "Accounts", status: statusWarn, detail: "none configured",
			fix: "add one with `ferry account add <name>`",
		}}
	}

	var checks []check
	for i := range accounts {
		a := &accounts[i]
		c := check{name: "Account " + a.Name}
		as := e.db.Account(a)

		switch _, err := e.sec.Get(secrets.APIKey(a.Name)); {
		case errors.Is(err, secrets.ErrNotFound):
			c.status = statusFail
			c.detail = "no Resend API key stored, so this account cannot sync or send"
			c.fix = fmt.Sprintf("run `ferry account add %s` again", a.Name)
			checks = append(checks, c)
			continue
		case err != nil:
			c.status = statusFail
			c.detail = "could not read the API key: " + err.Error()
			checks = append(checks, c)
			continue
		}

		if a.PasswordHash == "" {
			c.status = statusFail
			c.detail = "no app password set, so no mail client can log in"
			c.fix = fmt.Sprintf("run `ferry account passwd %s`", a.Name)
			checks = append(checks, c)
			continue
		}

		counts, err := as.Counts(ctx)
		if err != nil {
			c.status = statusFail
			c.detail = err.Error()
			checks = append(checks, c)
			continue
		}
		c.detail = fmt.Sprintf("%d messages, %s", counts.Messages, humanBytes(counts.Bytes))
		if len(a.Domains) == 0 {
			c.status = statusWarn
			c.detail += "; no verified sending domain, so sending is refused"
			c.fix = fmt.Sprintf("verify a domain in Resend, then run `ferry account refresh %s`", a.Name)
		}
		checks = append(checks, c)

		if repair {
			removed, freed, err := as.GCBlobs(ctx)
			gc := check{name: "Account " + a.Name + " storage"}
			switch {
			case err != nil:
				gc.status = statusWarn
				gc.detail = err.Error()
			case removed > 0:
				gc.detail = fmt.Sprintf("reclaimed %d orphaned message file(s), %s", removed, humanBytes(freed))
			default:
				gc.detail = "no orphaned message files"
			}
			checks = append(checks, gc)
		}
	}
	return checks
}

func (e *env) checkDaemon(ctx context.Context) check {
	c := check{name: "Daemon"}
	socket := e.cfg.ControlSocket()
	if !control.Available(ctx, socket) {
		c.status = statusWarn
		c.detail = "not running"
		c.fix = daemonStartHint()
		return c
	}
	st, err := control.Dial(socket).Status(ctx)
	if err != nil {
		c.status = statusWarn
		c.detail = "running but not answering: " + err.Error()
		return c
	}
	c.detail = fmt.Sprintf("running, up %s", st.Uptime)
	return c
}

// checkListeners connects to the mail servers exactly as a client would,
// including verifying the certificate, so a TLS problem shows up here rather
// than as an unexplained failure in Mail.
func (e *env) checkListeners(ctx context.Context) []check {
	socket := e.cfg.ControlSocket()
	if !control.Available(ctx, socket) {
		return nil
	}
	imapAddr, smtpAddr := e.effectiveAddrs(ctx)
	bundle, err := loadTLS(e.cfg)
	if err != nil {
		return nil
	}
	clientTLS, err := bundle.ClientConfig()
	if err != nil {
		return nil
	}

	var checks []check
	for _, l := range []struct {
		name string
		addr string
		off  bool
	}{
		{"IMAP listener", imapAddr, e.cfg.IMAP.Disabled},
		{"SMTP listener", smtpAddr, e.cfg.SMTP.Disabled},
	} {
		if l.off {
			continue
		}
		c := check{name: l.name, detail: l.addr}
		dialer := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 5 * time.Second},
			Config:    clientTLS,
		}
		conn, err := dialer.DialContext(ctx, "tcp", l.addr)
		if err != nil {
			c.status = statusFail
			c.detail = fmt.Sprintf("%s: %v", l.addr, err)
			c.fix = "check that the daemon is running and that nothing else holds this port"
		} else {
			_ = conn.Close()
			c.detail = l.addr + " accepting TLS connections"
		}
		checks = append(checks, c)
	}
	return checks
}

func daemonStartHint() string {
	if runtime.GOOS == "darwin" {
		return "start it with `brew services start ferry`, or run `ferry serve` in a terminal"
	}
	return "run `ferry serve`"
}
