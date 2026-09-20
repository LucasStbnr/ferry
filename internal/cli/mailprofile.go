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
		Long: `Writes a .mobileconfig file that sets up Apple Mail for Ferry's accounts:
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

			var profileAccounts []mobileconfig.Account
			for i := range accounts {
				a := &accounts[i]
				if len(wanted) > 0 && !wanted[a.Name] {
					continue
				}
				address := a.Address
				if address == "" {
					address = a.Name + "@localhost"
				}
				profileAccounts = append(profileAccounts, mobileconfig.Account{
					Name:        a.Name,
					DisplayName: a.Name,
					Address:     address,
					Password:    passwords[a.Name],
					IMAPHost:    imapHost,
					IMAPPort:    imapPort,
					SMTPHost:    smtpHost,
					SMTPPort:    smtpPort,
				})
			}
			if len(profileAccounts) == 0 {
				return fmt.Errorf("no matching accounts")
			}

			profile, err := mobileconfig.Build(mobileconfig.Options{
				Identifier:  "io.github.lucasstbnr.ferry",
				DisplayName: "Ferry",
				CACertPEM:   bundle.CACertPEM,
				Accounts:    profileAccounts,
			})
			if err != nil {
				return err
			}

			if outPath == "" {
				outPath = filepath.Join(e.cfg.Dir, "ferry.mobileconfig")
			}
			if outPath == "-" {
				_, err := e.out.Write(profile)
				return err
			}
			// A profile may carry a password, so it is never world-readable.
			if err := os.WriteFile(outPath, profile, 0o600); err != nil {
				return err
			}

			e.printf("Wrote %s\n", outPath)
			for _, a := range profileAccounts {
				note := ""
				if a.Password == "" {
					note = " (Mail will ask for the app password)"
				}
				e.printf("  %s → %s%s\n", a.Name, a.Address, note)
			}
			e.printf("\nInstall it: open the file, then approve it in\n")
			e.printf("System Settings → General → Device Management.\n")
			if runtime.GOOS == "darwin" && open {
				if err := exec.CommandContext(ctx, "open", outPath).Run(); err != nil {
					e.printf("\nCould not open it automatically: %v\n", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "where to write the profile (\"-\" for stdout; default: <data-dir>/ferry.mobileconfig)")
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
