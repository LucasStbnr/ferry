// Package mobileconfig writes an Apple configuration profile that sets up
// Ferry's accounts in Mail.
//
// Adding an IMAP account by hand means typing a hostname, two ports, a
// username and a generated password twice, and then separately trusting a
// certificate. A profile does all of it in one double-click, and it carries
// the CA certificate so Mail accepts the local TLS without a warning.
//
// The profile is unsigned. macOS therefore marks it "Unverified" during
// installation, which is expected: signing would need a certificate from
// Apple's developer program, and the profile is generated locally from data
// the user already has.
package mobileconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"strings"
)

// Account is one IMAP/SMTP account to configure.
type Account struct {
	// Name is the account name, used as the IMAP and SMTP username.
	Name string
	// DisplayName is what Mail shows in the sidebar.
	DisplayName string
	// Address is the From address.
	Address string
	// Password, when set, is embedded in the profile so Mail does not prompt.
	// It is left empty unless the user asks for it: a profile with a password
	// in it is a credential file, and it usually ends up in Downloads.
	Password string

	IMAPHost string
	IMAPPort int
	SMTPHost string
	SMTPPort int
}

// Options configure the profile.
type Options struct {
	// Identifier is the profile's reverse-DNS identifier.
	Identifier string
	// DisplayName and Description are shown during installation.
	DisplayName string
	Description string
	// Organization is shown as the profile's source.
	Organization string
	// CACertPEM is Ferry's local CA, installed so Mail trusts the daemon.
	CACertPEM []byte
	Accounts  []Account
}

// Build renders the profile as an Apple property list.
func Build(opts Options) ([]byte, error) {
	if len(opts.Accounts) == 0 {
		return nil, fmt.Errorf("mobileconfig: no accounts to configure")
	}
	if opts.Identifier == "" {
		opts.Identifier = "io.github.lucasstbnr.ferry"
	}
	if opts.DisplayName == "" {
		opts.DisplayName = "Ferry"
	}
	if opts.Organization == "" {
		opts.Organization = "Ferry"
	}
	if opts.Description == "" {
		opts.Description = "Configures Apple Mail to use the Ferry bridge for Resend mail."
	}

	var payloads []dict

	if len(opts.CACertPEM) > 0 {
		der, err := certDER(opts.CACertPEM)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, dict{
			"PayloadType":                "com.apple.security.root",
			"PayloadVersion":             1,
			"PayloadIdentifier":          opts.Identifier + ".ca",
			"PayloadUUID":                deterministicUUID(opts.Identifier + ".ca"),
			"PayloadDisplayName":         "Ferry local certificate authority",
			"PayloadDescription":         "Lets Mail verify Ferry's TLS certificate without a warning.",
			"PayloadCertificateFileName": "ferry-ca.crt",
			"PayloadContent":             data(der),
		})
	}

	for _, a := range opts.Accounts {
		id := opts.Identifier + ".mail." + a.Name
		display := a.DisplayName
		if display == "" {
			display = a.Name
		}
		p := dict{
			"PayloadType":        "com.apple.mail.managed",
			"PayloadVersion":     1,
			"PayloadIdentifier":  id,
			"PayloadUUID":        deterministicUUID(id),
			"PayloadDisplayName": display,
			"PayloadDescription": "Ferry account for " + a.Address,

			"EmailAccountDescription": display,
			"EmailAccountName":        display,
			"EmailAccountType":        "EmailTypeIMAP",
			"EmailAddress":            a.Address,

			"IncomingMailServerHostName":       a.IMAPHost,
			"IncomingMailServerPortNumber":     a.IMAPPort,
			"IncomingMailServerUseSSL":         true,
			"IncomingMailServerUsername":       a.Name,
			"IncomingMailServerAuthentication": "EmailAuthPassword",
			"IncomingMailServerIMAPPathPrefix": "",

			"OutgoingMailServerHostName":       a.SMTPHost,
			"OutgoingMailServerPortNumber":     a.SMTPPort,
			"OutgoingMailServerUseSSL":         true,
			"OutgoingMailServerUsername":       a.Name,
			"OutgoingMailServerAuthentication": "EmailAuthPassword",
			// The submission server is Ferry itself, so the same credentials
			// apply and Mail must not try to reuse some other account's.
			"OutgoingPasswordSameAsIncomingPassword": true,

			// Ferry is not a general-purpose mail host: keeping the account out
			// of other apps avoids surprises like Calendar trying to use it.
			"PreventAppSheet":           false,
			"PreventMove":               false,
			"SMIMEEnabled":              false,
			"allowMailDrop":             false,
			"disableMailRecentsSyncing": true,
		}
		if a.Password != "" {
			p["IncomingPassword"] = a.Password
			p["OutgoingPassword"] = a.Password
		}
		payloads = append(payloads, p)
	}

	root := dict{
		"PayloadType":              "Configuration",
		"PayloadVersion":           1,
		"PayloadIdentifier":        opts.Identifier,
		"PayloadUUID":              deterministicUUID(opts.Identifier),
		"PayloadDisplayName":       opts.DisplayName,
		"PayloadDescription":       opts.Description,
		"PayloadOrganization":      opts.Organization,
		"PayloadRemovalDisallowed": false,
		"PayloadScope":             "User",
		"PayloadContent":           payloads,
	}

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	buf.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	buf.WriteString(`<plist version="1.0">` + "\n")
	if err := writeValue(&buf, root, 0); err != nil {
		return nil, err
	}
	buf.WriteString("\n</plist>\n")
	return buf.Bytes(), nil
}

// certDER extracts the DER bytes from a PEM certificate, which is what the
// profile embeds.
func certDER(pemBytes []byte) ([]byte, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("mobileconfig: CA file is not a PEM certificate")
	}
	return block.Bytes, nil
}

// deterministicUUID derives a stable UUID from a string, so reinstalling a
// regenerated profile updates the existing one instead of adding a duplicate.
func deterministicUUID(seed string) string {
	sum := sha256.Sum256([]byte("ferry-mobileconfig:" + seed))
	h := hex.EncodeToString(sum[:16])
	return strings.ToUpper(fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]))
}

// data marks a []byte so writeValue emits a <data> element.
type data []byte

// dict is an ordered-on-output property list dictionary.
type dict map[string]any
