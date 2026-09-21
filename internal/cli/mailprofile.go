package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/LucasStbnr/ferry/internal/mobileconfig"
)

func newMailProfileCmd(e *env) *cobra.Command {
	var (
		outPath    string
		host       string
		open       bool
		withPasswd []string
	)
	cmd := &cobra.Command{
		Use:   "mail-profile [account...]",
		Short: "Write an Apple configuration profile (macOS and iOS only)",
		Long: `Writes a .mobileconfig file per account that sets up Apple Mail:
the right hostname, both ports, SSL on both, the username, and Ferry's CA
certificate so there is no certificate warning.

Double-click the file, then approve it in System Settings → General →
Device Management. The profile is unsigned, so macOS shows it as
"Unverified", which is expected for a profile generated on your own machine.

App passwords are not included unless you ask for them with
--with-password, because the file usually lands in Downloads and a profile
containing a password is a credential file. Without it, Mail asks for the
password once and stores it in the keychain.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := e.open(ctx); err != nil {
				return err
			}
			defer e.close()

			accounts, err := e.db.Accounts(ctx)
			if err != nil {
				return err
			}
			if len(accounts) == 0 {
				return fmt.Errorf("no accounts configured; add one with `ferry account add <name>`")
			}

			wanted := map[string]bool{}
			for _, a := range args {
				wanted[a] = true
			}
			passwords := map[string]string{}
			for _, spec := range withPasswd {
				name, password, found := strings.Cut(spec, "=")
				if !found {
					return fmt.Errorf("--with-password expects account=password, got %q", spec)
				}
				passwords[name] = password
			}

			bundle, err := loadTLS(e.cfg)
			if err != nil {
				return err
			}

			imapAddr, smtpAddr := e.effectiveAddrs(ctx)
			imapHost, imapPort, err := splitHostPort(imapAddr, host)
			if err != nil {
				return fmt.Errorf("imap address: %w", err)
			}
			smtpHost, smtpPort, err := splitHostPort(smtpAddr, host)
			if err != nil {
				return fmt.Errorf("smtp address: %w", err)
			}

			type built struct {
				account string
				path    string
				address string
				hasPass bool
				data    []byte
			}
			var profiles []built

			// One profile per account, each with its own identifier.
			//
			// A single profile holding every account looks tidier and is a
			// trap: installing a profile whose identifier already exists
			// replaces it, and macOS removes the old one first, taking its
			// mail accounts with it. Adding a second account would therefore
			// tear down the first and ask for every password again. Separate
			// identifiers keep each account independent.
			for i := range accounts {
				a := &accounts[i]
				if len(wanted) > 0 && !wanted[a.Name] {
					continue
				}
				address := a.Address
				if address == "" {
					address = a.Name + "@localhost"
				}
				profile, err := mobileconfig.Build(mobileconfig.Options{
					Identifier:  "io.github.lucasstbnr.ferry." + a.Name,
					DisplayName: "Ferry (" + a.Name + ")",
					Description: "Configures this mail account to read " + address + " through Ferry.",
					CACertPEM:   bundle.CACertPEM,
					Accounts: []mobileconfig.Account{{
						Name:        a.Name,
						DisplayName: a.Name,
						SenderName:  a.DisplayName,
						Address:     address,
						Password:    passwords[a.Name],
						IMAPHost:    imapHost,
						IMAPPort:    imapPort,
						SMTPHost:    smtpHost,
						SMTPPort:    smtpPort,
					}},
				})
				if err != nil {
					return err
				}
				profiles = append(profiles, built{
					account: a.Name,
					address: address,
					hasPass: passwords[a.Name] != "",
					data:    profile,
				})
			}
			if len(profiles) == 0 {
				return fmt.Errorf("no matching accounts")
			}

			if outPath == "-" {
				if len(profiles) != 1 {
					return fmt.Errorf("writing to stdout needs exactly one account; name one, or drop -o")
				}
				_, err := e.out.Write(profiles[0].data)
				return err
			}
			if outPath != "" && len(profiles) != 1 {
				return fmt.Errorf("-o names a single file but %d accounts matched; name one account, or drop -o to write one profile per account",
					len(profiles))
			}

			for i := range profiles {
				path := outPath
				if path == "" {
					path = filepath.Join(e.cfg.Dir, "ferry-"+profiles[i].account+".mobileconfig")
				}
				// A profile may carry a password, so it is never world-readable.
				if err := os.WriteFile(path, profiles[i].data, 0o600); err != nil {
					return err
				}
				profiles[i].path = path
			}

			e.printf("Wrote %d profile(s), one per account:\n\n", len(profiles))
			for _, pr := range profiles {
				note := " (the client will ask for the app password)"
				if pr.hasPass {
					note = " (password embedded)"
				}
				e.printf("  %s\n    %s%s\n", pr.path, pr.address, note)
			}
			e.printf("\nInstall each one: open the file, then approve it in\n")
			e.printf("System Settings → General → Device Management.\n")
			e.printf("\nEach account is a separate profile, so installing or removing one\n")
			e.printf("leaves the others alone.\n")

			if runtime.GOOS == "darwin" && open {
				for _, pr := range profiles {
					if err := exec.CommandContext(ctx, "open", pr.path).Run(); err != nil {
						e.printf("\nCould not open %s automatically: %v\n", pr.path, err)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&outPath, "output", "o", "",
		"write a single account's profile here (\"-\" for stdout); default: <data-dir>/ferry-<account>.mobileconfig")
	cmd.Flags().StringVar(&host, "host", "", "hostname Mail should connect to (default: the configured listen address, or localhost)")
	cmd.Flags().BoolVar(&open, "open", false, "open the profile after writing it")
	cmd.Flags().StringArrayVar(&withPasswd, "with-password", nil, "embed an app password, as account=password (the file then contains a credential)")
	return cmd
}

// splitHostPort turns a listen address into what a client should connect to.
// A wildcard bind is not a usable hostname, so it becomes localhost unless the
// caller names one.
func splitHostPort(addr, override string) (host string, port int, err error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	if port, err = strconv.Atoi(portStr); err != nil {
		return "", 0, fmt.Errorf("port %q is not a number", portStr)
	}
	if override != "" {
		return override, port, nil
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]", "127.0.0.1", "::1":
		return "localhost", port, nil
	}
	return host, port, nil
}
