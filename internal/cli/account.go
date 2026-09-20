package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/LucasStbnr/ferry/internal/account"
	"github.com/LucasStbnr/ferry/internal/control"
	"github.com/LucasStbnr/ferry/internal/secrets"
)

func newAccountCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "Add, list and remove Resend accounts",
	}
	cmd.AddCommand(
		newAccountAddCmd(e),
		newAccountListCmd(e),
		newAccountRemoveCmd(e),
		newAccountPasswdCmd(e),
		newAccountSetKeyCmd(e),
		newAccountIdentityCmd(e),
		newAccountRefreshCmd(e),
		newAccountWebhookCmd(e),
	)
	return cmd
}

func newAccountAddCmd(e *env) *cobra.Command {
	var (
		apiKey      string
		address     string
		displayName string
		password    string
		noSync      bool
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a Resend account and generate its app password",
		Long: `Adds a Resend account.

The API key is read from the terminal unless --api-key is given, so it does
not end up in the shell history. It is stored in the system keyring, never in
Ferry's database, and never written to a log.

Ferry generates an app password for this account and prints it once. That is
the password your mail client uses for both IMAP and SMTP; Ferry keeps only a hash
of it, so a lost password has to be replaced with ` + "`ferry account passwd`" + `.

The full history is downloaded in the background from the moment the account
is added, because Resend's raw-message and attachment links expire.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := e.open(ctx); err != nil {
				return err
			}
			defer e.close()

			name := args[0]
			if apiKey == "" {
				var err error
				if apiKey, err = promptSecret(e, "Resend API key: "); err != nil {
					return err
				}
			}
			if strings.TrimSpace(apiKey) == "" {
				return errors.New("no API key given")
			}

			created, err := e.mgr.Add(ctx, account.AddOptions{
				Name:        name,
				APIKey:      apiKey,
				Address:     address,
				DisplayName: displayName,
				Password:    password,
			})
			if err != nil {
				return err
			}

			e.printf("Account %q added.\n\n", name)
			e.printf("  Sends as        %s\n",
				formatSender(created.Account.DisplayName, created.Account.Address, name))
			if len(created.Domains) > 0 {
				e.printf("  Sending domains %s\n", strings.Join(created.Domains, ", "))
			} else {
				e.printf("  Sending domains none verified yet; sending will be refused until one is\n")
			}
			e.printf("  IMAP            %s (SSL/TLS)\n", e.cfg.IMAP.Addr)
			e.printf("  SMTP            %s (SSL/TLS)\n", e.cfg.SMTP.Addr)
			e.printf("  Username        %s\n", name)
			e.printf("  App password    %s\n", created.Password)
			e.printf("\nThis password is shown once and cannot be recovered. " +
				"Run `ferry account passwd` to replace it.\n")
			e.printf("\nNext: run `ferry trust` so clients accept Ferry's certificate, then\n")
			e.printf("configure your mail client with the settings above.\n")
			if runtime.GOOS == "darwin" {
				e.printf("For Apple Mail, `ferry mail-profile --open` does it in one step.\n")
			}

			if !noSync {
				notifyDaemonReload(ctx, e)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&apiKey, "api-key", "", "Resend API key (prompted for if omitted, which keeps it out of shell history)")
	cmd.Flags().StringVar(&address, "address", "", "email address to use as From (default: hello@<first verified domain>)")
	cmd.Flags().StringVar(&displayName, "display-name", "", `name shown beside the address on outgoing mail, e.g. "Acme Support"`)
	cmd.Flags().StringVar(&password, "password", "", "set the app password instead of generating one")
	cmd.Flags().BoolVar(&noSync, "no-reload", false, "do not tell a running daemon about the new account")
	return cmd
}

func newAccountListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured accounts",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := e.open(ctx); err != nil {
				return err
			}
			defer e.close()

			statuses, err := e.mgr.Status(ctx)
			if err != nil {
				return err
			}
			if len(statuses) == 0 {
				e.printf("No accounts yet. Add one with `ferry account add <name>`.\n")
				return nil
			}

			w := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tADDRESS\tDOMAINS\tMESSAGES\tUNREAD\tAPI KEY")
			for _, s := range statuses {
				key := "missing"
				if s.HasAPIKey {
					key = "ok"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\n",
					s.Name, orNone(s.Address), orNone(strings.Join(s.Domains, ",")),
					s.Counts.Messages, s.Counts.Unseen, key)
			}
			return w.Flush()
		},
	}
}

func newAccountRemoveCmd(e *env) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove an account and its locally stored mail",
		Long: `Removes an account from Ferry.

This deletes Ferry's local copy of the account's mail, its app password and
its stored API key. It does not touch anything in Resend: the messages remain
there and can be downloaded again by adding the account back.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := e.open(ctx); err != nil {
				return err
			}
			defer e.close()

			name := args[0]
			acct, err := e.db.AccountByName(ctx, name)
			if err != nil {
				return err
			}
			counts, err := e.db.Account(acct).Counts(ctx)
			if err != nil {
				return err
			}

			if !force {
				e.printf("This removes account %q and its %d locally stored messages (%s).\n",
					name, counts.Messages, humanBytes(counts.Bytes))
				e.printf("Nothing in Resend is deleted.\n")
				if !confirm(e, fmt.Sprintf("Type %q to confirm: ", name), name) {
					e.printf("Cancelled.\n")
					return nil
				}
			}

			if err := e.mgr.Remove(ctx, name); err != nil {
				return err
			}
			e.printf("Account %q removed.\n", name)
			notifyDaemonReload(ctx, e)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "do not ask for confirmation")
	return cmd
}

func newAccountPasswdCmd(e *env) *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "passwd <name>",
		Short: "Generate a new app password for an account",
		Long: `Replaces the account's app password and prints the new one.

Any mail client configured with the old password stops working immediately,
so update your mail client (or, on macOS, reinstall the profile from
` + "`ferry mail-profile`" + `) right afterwards.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := e.open(ctx); err != nil {
				return err
			}
			defer e.close()

			newPassword, err := e.mgr.ResetPassword(ctx, args[0], password)
			if err != nil {
				return err
			}
			e.printf("New app password for %q: %s\n", args[0], newPassword)
			e.printf("\nUpdate your mail client with this password. It is shown once.\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&password, "password", "", "set this password instead of generating one")
	return cmd
}

func newAccountSetKeyCmd(e *env) *cobra.Command {
	var apiKey string
	cmd := &cobra.Command{
		Use:     "set-key <name>",
		Aliases: []string{"key"},
		Short:   "Replace an account's Resend API key",
		Long: `Stores a new Resend API key for an existing account.

Use this to rotate a key, or to restore one that was lost: a key revoked in
Resend, or a credential store that was cleared. Nothing else about the account
changes: the mail already downloaded, the folders, the read state and the app
password your mail client uses all stay exactly as they are, so there is no
need to reconfigure the client.

The key is verified against Resend before it is stored, and the account's
verified sending domains are refreshed at the same time.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := e.open(ctx); err != nil {
				return err
			}
			defer e.close()

			name := args[0]
			if _, err := e.db.AccountByName(ctx, name); err != nil {
				return err
			}
			if apiKey == "" {
				var err error
				if apiKey, err = promptSecret(e, "Resend API key: "); err != nil {
					return err
				}
			}
			if strings.TrimSpace(apiKey) == "" {
				return errors.New("no API key given")
			}

			domains, err := e.mgr.SetAPIKey(ctx, name, apiKey)
			if err != nil {
				return err
			}

			e.printf("API key updated for %q.\n", name)
			if len(domains) > 0 {
				e.printf("Sending domains: %s\n", strings.Join(domains, ", "))
			} else {
				e.printf("No verified sending domains; sending will be refused until one is verified.\n")
			}
			e.printf("\nNothing else changed; your mail client keeps working with the same password.\n")
			notifyDaemonReload(ctx, e)
			return nil
		},
	}
	cmd.Flags().StringVar(&apiKey, "api-key", "", "the new API key (prompted for if omitted)")
	return cmd
}

func newAccountRefreshCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "refresh <name>",
		Short: "Re-read the account's verified sending domains from Resend",
		Long: `Asks Resend which domains this account may send from and stores the answer.

Run this after verifying a new domain, otherwise Ferry keeps refusing to send
from it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := e.open(ctx); err != nil {
				return err
			}
			defer e.close()

			domains, err := e.mgr.RefreshDomains(ctx, args[0])
			if err != nil {
				return err
			}
			if len(domains) == 0 {
				e.printf("%s has no verified sending domains. Verify one in Resend, then run this again.\n", args[0])
				return nil
			}
			e.printf("%s can send from: %s\n", args[0], strings.Join(domains, ", "))
			notifyDaemonReload(ctx, e)
			return nil
		},
	}
}

func newAccountWebhookCmd(e *env) *cobra.Command {
	var (
		secret string
		remove bool
	)
	cmd := &cobra.Command{
		Use:   "webhook <name>",
		Short: "Store the Resend webhook signing secret for an account",
		Long: `Stores the signing secret Resend shows when you create a webhook endpoint.

With a secret stored and a webhook address configured, new mail appears in
your mail client as soon as Resend delivers the event, and bounces and spam
complaints are filed into the Inbox as delivery notices.

Every webhook request must carry a valid signature; there is no way to turn
verification off, because an unsigned endpoint would let anyone who found the
URL inject messages into the Inbox.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := e.open(ctx); err != nil {
				return err
			}
			defer e.close()

			name := args[0]
			if _, err := e.db.AccountByName(ctx, name); err != nil {
				return err
			}

			if remove {
				if err := e.sec.Delete(secrets.WebhookSecret(name)); err != nil {
					return err
				}
				e.printf("Webhook secret for %q removed; this account now syncs by polling only.\n", name)
				notifyDaemonReload(ctx, e)
				return nil
			}

			if secret == "" {
				var err error
				if secret, err = promptSecret(e, "Resend webhook signing secret (whsec_…): "); err != nil {
					return err
				}
			}
			if strings.TrimSpace(secret) == "" {
				return errors.New("no signing secret given")
			}
			if err := e.sec.Set(secrets.WebhookSecret(name), strings.TrimSpace(secret)); err != nil {
				return err
			}

			e.printf("Webhook secret stored for %q.\n", name)
			if e.cfg.Webhook.Addr == "" {
				e.printf("\nThe webhook receiver is not enabled. Set \"webhook\": {\"addr\": \":8443\"} in\n%s\nand restart the daemon.\n",
					e.cfg.Path("config.json"))
			} else {
				e.printf("Point the Resend endpoint at %s%s/%s\n", e.cfg.Webhook.Addr, e.cfg.Webhook.Path, name)
			}
			notifyDaemonReload(ctx, e)
			return nil
		},
	}
	cmd.Flags().StringVar(&secret, "secret", "", "the signing secret (prompted for if omitted)")
	cmd.Flags().BoolVar(&remove, "remove", false, "remove the stored secret")
	return cmd
}

// notifyDaemonReload tells a running daemon that the account list changed. A
// daemon that is not running is not an error: it will read the new state when
// it starts.
func notifyDaemonReload(ctx context.Context, e *env) {
	socket := e.cfg.ControlSocket()
	if !control.Available(ctx, socket) {
		return
	}
	if err := control.Dial(socket).Reload(ctx); err != nil {
		e.printf("Note: the running daemon did not reload (%v). Restart it to pick up the change.\n", err)
		return
	}
	e.printf("The running daemon picked up the change.\n")
}

// promptSecret reads a secret without echoing it.
func promptSecret(e *env, prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Piped input: read a line so scripts and `ferry account add < key` work.
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read secret from stdin: %w", err)
		}
		return strings.TrimSpace(line), nil
	}
	_, _ = fmt.Fprint(e.err, prompt)
	value, err := term.ReadPassword(fd)
	_, _ = fmt.Fprintln(e.err)
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return strings.TrimSpace(string(value)), nil
}

// confirm asks the user to type an exact phrase. Anything else, including a
// closed stdin, means no.
func confirm(e *env, prompt, want string) bool {
	_, _ = fmt.Fprint(e.err, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	return strings.TrimSpace(line) == want
}

func orNone(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func newAccountIdentityCmd(e *env) *cobra.Command {
	var (
		address     string
		displayName string
	)
	cmd := &cobra.Command{
		Use:   "identity <name>",
		Short: "Change the From address and sender name on outgoing mail",
		Long: `Sets how your mail appears to the people who receive it: the From address,
and the name shown beside it.

  ferry account identity mysite --address contact@example.com
  ferry account identity mysite --display-name "Acme Support"

The address must be on a domain the account can send from, since the SMTP
server checks that on every message.

A mail client configured from a profile treats these settings as managed and
will not let you edit them itself, so change them here and reinstall the
profile:

  ferry mail-profile --open

Sending from a different address occasionally does not need this at all: any
address on a verified domain is accepted, so you can add aliases in the client
and pick between them when composing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := e.open(ctx); err != nil {
				return err
			}
			defer e.close()

			name := args[0]
			if address == "" && displayName == "" {
				acct, err := e.db.AccountByName(ctx, name)
				if err != nil {
					return err
				}
				e.printf("Outgoing mail from %q currently appears as:\n\n", name)
				e.printf("  From  %s\n", formatSender(acct.DisplayName, acct.Address, acct.Name))
				e.printf("\nChange it with --address and --display-name.\n")
				return nil
			}

			acct, err := e.mgr.SetIdentity(ctx, name, address, displayName)
			if err != nil {
				return err
			}
			e.printf("Outgoing mail from %q will now appear as:\n\n", name)
			e.printf("  From  %s\n", formatSender(acct.DisplayName, acct.Address, acct.Name))
			e.printf("\nYour mail client keeps its current settings until it is reconfigured.\n")
			if runtime.GOOS == "darwin" {
				e.printf("For Apple Mail, run `ferry mail-profile --open` and reinstall the profile.\n")
			}
			notifyDaemonReload(ctx, e)
			return nil
		},
	}
	cmd.Flags().StringVar(&address, "address", "", "the From address, e.g. contact@example.com")
	cmd.Flags().StringVar(&displayName, "display-name", "", `the name shown beside the address, e.g. "Acme Support"`)
	return cmd
}

// formatSender renders a From header the way a recipient sees it.
func formatSender(displayName, address, fallback string) string {
	if displayName == "" {
		displayName = fallback
	}
	if address == "" {
		return displayName
	}
	return fmt.Sprintf("%s <%s>", displayName, address)
}
