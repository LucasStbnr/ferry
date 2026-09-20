package cli

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

func newTrustCmd(e *env) *cobra.Command {
	var (
		system    bool
		printOnly bool
	)
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Trust Ferry's local certificate authority",
		Long: `Adds Ferry's generated CA certificate to the macOS keychain, so Mail accepts
the local IMAP and SMTP connections without a certificate warning.

By default it goes into the login keychain, which needs no administrator
rights and applies to the current user only. --system installs it for every
user on the machine and will ask for your password.

This trusts one certificate that Ferry generated on this machine and whose
private key never leaves the data directory. It is not a certificate any
website or third party can use.

Self-hosting with a real certificate does not need this at all.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := e.loadConfig(); err != nil {
				return err
			}
			bundle, err := loadTLS(e.cfg)
			if err != nil {
				return err
			}
			if e.cfg.TLS.CertFile != "" {
				e.printf("This installation uses the certificate at %s.\n", e.cfg.TLS.CertFile)
				e.printf("Nothing to trust: clients verify it through the usual certificate chain.\n")
				return nil
			}

			caPath := bundle.CACertPath()
			if printOnly {
				data, err := os.ReadFile(caPath)
				if err != nil {
					return err
				}
				_, err = e.out.Write(data)
				return err
			}

			if runtime.GOOS != "darwin" {
				e.printf("Ferry's CA certificate is at:\n  %s\n\n", caPath)
				e.printf("Add it to this system's trust store to connect without warnings.\n")
				e.printf("On Debian and Ubuntu:\n")
				e.printf("  sudo cp %s /usr/local/share/ca-certificates/ferry.crt && sudo update-ca-certificates\n", caPath)
				return nil
			}

			keychain, args := loginKeychainArgs(caPath)
			if system {
				keychain, args = systemKeychainArgs(caPath)
				e.printf("Installing into the system keychain; macOS will ask for your password.\n")
			}

			out, err := exec.CommandContext(cmd.Context(), "security", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("could not add the certificate to %s: %w\n%s", keychain, err, strings.TrimSpace(string(out)))
			}

			e.printf("Ferry's certificate authority is now trusted in the %s keychain.\n", keychain)
			e.printf("Apple Mail will connect to %s and %s without a warning.\n", e.cfg.IMAP.Addr, e.cfg.SMTP.Addr)
			e.printf("\nTo undo this:\n  security delete-certificate -c \"Ferry local CA\"\n")
			return nil
		},
	}
	cmd.Flags().BoolVar(&system, "system", false, "trust for every user on this machine (asks for your password)")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the CA certificate instead of installing it")
	return cmd
}

// loginKeychainArgs trusts the CA for the current user only, which needs no
// administrator rights. It returns the keychain's name, for the message shown
// afterwards, and the `security` arguments.
func loginKeychainArgs(caPath string) (keychain string, args []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return "login", []string{
		"add-trusted-cert",
		"-r", "trustRoot",
		"-k", home + "/Library/Keychains/login.keychain-db",
		caPath,
	}
}

// systemKeychainArgs trusts the CA for every user, which macOS will ask for a
// password to authorise.
func systemKeychainArgs(caPath string) (keychain string, args []string) {
	return "system", []string{
		"add-trusted-cert",
		"-d",
		"-r", "trustRoot",
		"-k", "/Library/Keychains/System.keychain",
		caPath,
	}
}
