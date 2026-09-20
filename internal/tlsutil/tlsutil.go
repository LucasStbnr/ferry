// Package tlsutil gives Ferry a certificate without asking the user to obtain
// one.
//
// Mail insists on TLS, and Ferry never serves IMAP or SMTP in the clear even on
// loopback: an app password on a local socket is still a password on a socket
// any process on the machine could read. So on first run Ferry generates a
// private CA and a leaf certificate for localhost, keeps both in the data
// directory, and `ferry trust` adds the CA to the login keychain so Mail
// accepts it without a warning.
//
// Self-hosting instead supplies a real certificate through the config file, in
// which case none of this runs.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Lifetimes. The leaf is short enough that a leaked key is not a long-term
// problem and long enough that nobody has to think about it; Ferry renews it
// automatically when it is close to expiring.
const (
	caLifetime     = 10 * 365 * 24 * time.Hour
	leafLifetime   = 825 * 24 * time.Hour // the maximum Apple platforms accept
	renewThreshold = 30 * 24 * time.Hour
)

// File names inside the TLS directory.
const (
	caCertFile   = "ca.crt"
	caKeyFile    = "ca.key"
	leafCertFile = "server.crt"
	leafKeyFile  = "server.key"
)

// Bundle is a CA and the leaf certificate it signed.
type Bundle struct {
	Dir string
	// CACertPEM is the certificate a client must trust.
	CACertPEM []byte
	// Hostnames are the names the leaf is valid for.
	Hostnames []string

	cert tls.Certificate
}

// EnsureBundle loads the CA and leaf from dir, creating or renewing whatever is
// missing or stale. hostnames are added to the leaf alongside localhost.
func EnsureBundle(dir string, hostnames []string) (*Bundle, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("tlsutil: create %s: %w", dir, err)
	}

	caCert, caKey, err := loadOrCreateCA(dir)
	if err != nil {
		return nil, err
	}

	names := normaliseHostnames(hostnames)
	leaf := loadLeaf(dir, names)
	if leaf == nil {
		var err error
		if leaf, err = createLeaf(dir, caCert, caKey, names); err != nil {
			return nil, err
		}
	}

	caPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, err
	}
	return &Bundle{Dir: dir, CACertPEM: caPEM, Hostnames: names, cert: *leaf}, nil
}

// LoadBundle uses a certificate the operator supplied instead of a generated
// one. Self-hosting behind a real hostname takes this path.
func LoadBundle(certFile, keyFile string) (*Bundle, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: load certificate: %w", err)
	}
	b := &Bundle{cert: cert}
	if len(cert.Certificate) > 0 {
		if parsed, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			b.Hostnames = parsed.DNSNames
		}
	}
	return b, nil
}

// ServerConfig returns a TLS configuration for the IMAP and SMTP listeners.
func (b *Bundle) ServerConfig() (*tls.Config, error) {
	if len(b.cert.Certificate) == 0 {
		return nil, errors.New("tlsutil: no certificate loaded")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{b.cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientConfig returns a configuration that trusts this bundle's CA. It is
// what `ferry doctor` and the tests connect with; a mail client instead
// trusts the CA through the system trust store.
func (b *Bundle) ClientConfig() (*tls.Config, error) {
	pool := x509.NewCertPool()
	if len(b.CACertPEM) > 0 {
		if !pool.AppendCertsFromPEM(b.CACertPEM) {
			return nil, errors.New("tlsutil: CA certificate is not valid PEM")
		}
	} else if len(b.cert.Certificate) > 0 {
		parsed, err := x509.ParseCertificate(b.cert.Certificate[0])
		if err != nil {
			return nil, err
		}
		pool.AddCert(parsed)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, ServerName: "localhost"}, nil
}

// CACertPath is where the CA certificate lives, for `ferry trust` and for the
// configuration profile.
func (b *Bundle) CACertPath() string { return filepath.Join(b.Dir, caCertFile) }

// LeafNotAfter reports when the server certificate expires.
func (b *Bundle) LeafNotAfter() (time.Time, error) {
	if len(b.cert.Certificate) == 0 {
		return time.Time{}, errors.New("tlsutil: no certificate loaded")
	}
	parsed, err := x509.ParseCertificate(b.cert.Certificate[0])
	if err != nil {
		return time.Time{}, err
	}
	return parsed.NotAfter, nil
}

func normaliseHostnames(extra []string) []string {
	set := map[string]bool{"localhost": true}
	for _, h := range extra {
		if h != "" {
			set[h] = true
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func loadOrCreateCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPath := filepath.Join(dir, caCertFile)
	keyPath := filepath.Join(dir, caKeyFile)

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		cert, key, err := parsePair(certPEM, keyPEM)
		if err == nil && time.Now().Before(cert.NotAfter) {
			return cert, key, nil
		}
		// A corrupt or expired CA is replaced; the old one is left on disk so
		// nothing the user trusted is destroyed without them noticing.
	}
	if certErr != nil && !errors.Is(certErr, fs.ErrNotExist) {
		return nil, nil, certErr
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Ferry local CA",
			Organization: []string{"Ferry"},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	if err := writePair(certPath, keyPath, der, key); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// loadLeaf returns the stored leaf certificate when it is still usable, and
// nil when one must be issued.
//
// It reports no error, because there is nothing a caller could do differently:
// missing, unreadable, corrupt, expiring and "does not cover the names we now
// want" all mean the same thing, which is that Ferry issues a fresh one.
func loadLeaf(dir string, hostnames []string) *tls.Certificate {
	certPEM, err := os.ReadFile(filepath.Join(dir, leafCertFile))
	if err != nil {
		return nil
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, leafKeyFile))
	if err != nil {
		return nil
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(cert.Certificate) == 0 {
		return nil
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil
	}
	if time.Now().Add(renewThreshold).After(parsed.NotAfter) {
		return nil // due for renewal
	}
	for _, want := range hostnames {
		if parsed.VerifyHostname(want) != nil {
			return nil // a name was added since this leaf was issued
		}
	}
	return &cert
}

func createLeaf(dir string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, hostnames []string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostnames[0], Organization: []string{"Ferry"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     hostnames,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}

	certPath := filepath.Join(dir, leafCertFile)
	keyPath := filepath.Join(dir, leafKeyFile)
	if err := writePair(certPath, keyPath, der, key); err != nil {
		return nil, err
	}

	// The chain served to clients includes the CA, so a client that already
	// trusts the CA needs nothing else to verify the leaf.
	cert := tls.Certificate{
		Certificate: [][]byte{der, caCert.Raw},
		PrivateKey:  key,
	}
	return &cert, nil
}

func writePair(certPath, keyPath string, der []byte, key *ecdsa.PrivateKey) error {
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	// The private key is readable only by the user who runs Ferry.
	return os.WriteFile(keyPath, keyPEM, 0o600)
}

func parsePair(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, errors.New("tlsutil: malformed PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}
