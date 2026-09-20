package tlsutil_test

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LucasStbnr/ferry/internal/tlsutil"
)

func TestEnsureBundleGeneratesAndReuses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")

	first, err := tlsutil.EnsureBundle(dir, nil)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatal(err)
	}

	// The private keys must not be readable by other users.
	for _, name := range []string{"ca.key", "server.key"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s is mode %04o", name, perm)
		}
	}

	second, err := tlsutil.EnsureBundle(dir, nil)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if string(first.CACertPEM) != string(second.CACertPEM) {
		t.Fatal("a second call regenerated the CA; every client would have to trust it again")
	}
}

func TestNewHostnameReissuesTheLeaf(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if _, err := tlsutil.EnsureBundle(dir, nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "server.crt"))
	if err != nil {
		t.Fatal(err)
	}

	bundle, err := tlsutil.EnsureBundle(dir, []string{"mail.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("asking for a new hostname did not reissue the certificate")
	}
	// The CA is stable even when the leaf changes.
	if len(bundle.CACertPEM) == 0 {
		t.Fatal("CA certificate missing")
	}
}

// TestHandshake is the check that matters: a client that trusts the CA must be
// able to complete a TLS handshake against the server config, for localhost
// and for 127.0.0.1, because a client may be configured with either.
func TestHandshake(t *testing.T) {
	bundle, err := tlsutil.EnsureBundle(filepath.Join(t.TempDir(), "tls"), nil)
	if err != nil {
		t.Fatal(err)
	}
	serverCfg, err := bundle.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	clientCfg, err := bundle.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.WriteString(conn, "* OK ferry\r\n")
			}()
		}
	}()

	// Mail may reach the daemon by either name, so both must verify.
	for _, serverName := range []string{"localhost", "127.0.0.1"} {
		cfg := clientCfg.Clone()
		cfg.ServerName = serverName
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", ln.Addr().String(), cfg)
		if err != nil {
			t.Fatalf("handshake as %q failed: %v", serverName, err)
		}
		conn.Close()
	}
}

func TestUntrustedClientIsRejected(t *testing.T) {
	bundle, err := tlsutil.EnsureBundle(filepath.Join(t.TempDir(), "tls"), nil)
	if err != nil {
		t.Fatal(err)
	}
	serverCfg, _ := bundle.ServerConfig()

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()

	// A client with an empty trust store must not accept Ferry's certificate:
	// if it did, the local CA would be pointless.
	_, err = tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", ln.Addr().String(),
		&tls.Config{RootCAs: x509.NewCertPool(), ServerName: "localhost"})
	if err == nil {
		t.Fatal("a client that trusts nothing accepted the certificate")
	}
}

func TestCertificateChainIncludesTheCA(t *testing.T) {
	bundle, err := tlsutil.EnsureBundle(filepath.Join(t.TempDir(), "tls"), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := bundle.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Serving the CA alongside the leaf means a client that already trusts the
	// CA needs nothing else to build the chain.
	if n := len(cfg.Certificates[0].Certificate); n < 2 {
		t.Fatalf("the served chain has %d certificate(s), want the leaf and the CA", n)
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2 or better", cfg.MinVersion)
	}
}

func TestLeafLifetimeIsAcceptedByApplePlatforms(t *testing.T) {
	bundle, err := tlsutil.EnsureBundle(filepath.Join(t.TempDir(), "tls"), nil)
	if err != nil {
		t.Fatal(err)
	}
	notAfter, err := bundle.LeafNotAfter()
	if err != nil {
		t.Fatal(err)
	}
	// Apple rejects a server certificate valid for more than 825 days.
	if days := time.Until(notAfter).Hours() / 24; days > 825 {
		t.Fatalf("the leaf is valid for %.0f days; Apple platforms reject anything over 825", days)
	}
	if time.Until(notAfter) < 30*24*time.Hour {
		t.Fatal("a fresh certificate should not already be near renewal")
	}
}

func TestLoadBundleUsesASuppliedCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if _, err := tlsutil.EnsureBundle(dir, []string{"mail.example.test"}); err != nil {
		t.Fatal(err)
	}

	bundle, err := tlsutil.LoadBundle(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := bundle.ServerConfig(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range bundle.Hostnames {
		if h == "mail.example.test" {
			found = true
		}
	}
	if !found {
		t.Errorf("hostnames = %v", bundle.Hostnames)
	}
}

func TestLoadBundleRejectsMissingFiles(t *testing.T) {
	if _, err := tlsutil.LoadBundle("/nonexistent/cert.pem", "/nonexistent/key.pem"); err == nil {
		t.Fatal("a missing certificate was accepted")
	}
}
