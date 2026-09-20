// Package config resolves where Ferry keeps its state and what the daemon
// listens on. Defaults are deliberately conservative: loopback-only binds and
// unprivileged ports, so a fresh install cannot expose mail to the network.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// EnvDataDir overrides the data directory. The Docker image sets it to /data.
const EnvDataDir = "FERRY_DATA_DIR"

// Default listen addresses. Unprivileged ports on loopback so that Ferry runs
// as a normal user and is unreachable from the network.
const (
	DefaultIMAPAddr    = "127.0.0.1:1993"
	DefaultSMTPAddr    = "127.0.0.1:1465"
	DefaultWebhookAddr = ""
	DefaultControlSock = "ferry.sock"
)

// Config is the on-disk daemon configuration, stored as config.json in the
// data directory. Every field has a working default, so the file is optional.
type Config struct {
	// Dir is the data directory. It is set by Load and never serialised.
	Dir string `json:"-"`

	IMAP    Listener `json:"imap"`
	SMTP    Listener `json:"smtp"`
	Webhook Webhook  `json:"webhook"`
	Sync    Sync     `json:"sync"`
	TLS     TLS      `json:"tls"`

	// LogLevel is one of debug, info, warn, error.
	LogLevel string `json:"log_level"`
	// LogFormat is text or json.
	LogFormat string `json:"log_format"`
}

// Listener is a server bind address.
type Listener struct {
	Addr string `json:"addr"`
	// Disabled turns the listener off entirely.
	Disabled bool `json:"disabled"`
}

// Webhook configures the optional Resend webhook receiver. It is off unless
// Addr is set, and it always requires a signing secret.
type Webhook struct {
	Addr string `json:"addr"`
	// Path is the URL path the receiver answers on.
	Path string `json:"path"`
	// TLS serves the receiver over HTTPS using the daemon certificate.
	// Leave it off when a reverse proxy terminates TLS.
	TLS bool `json:"tls"`
}

// Sync controls the background poller.
type Sync struct {
	// Interval between polls. Zero disables polling; Ferry then only syncs on
	// demand or from webhooks.
	Interval Duration `json:"interval"`
	// RequestsPerSecond caps API traffic. Resend allows 10/s per team; the
	// default leaves half the budget to the website using the same account.
	RequestsPerSecond float64 `json:"requests_per_second"`
	// BackfillPageSize is how many list rows are requested at a time.
	BackfillPageSize int `json:"backfill_page_size"`
	// MaxMessageBytes caps a single stored message.
	MaxMessageBytes int64 `json:"max_message_bytes"`
}

// TLS selects the daemon certificate. With no fields set, Ferry generates a
// local CA and a leaf for localhost and trusts it via `ferry trust`.
type TLS struct {
	// CertFile and KeyFile use a certificate you supply (self-hosting behind a
	// real hostname). Both must be set together.
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	// Hostnames are added to the generated leaf certificate.
	Hostnames []string `json:"hostnames"`
}

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		IMAP: Listener{Addr: DefaultIMAPAddr},
		SMTP: Listener{Addr: DefaultSMTPAddr},
		Webhook: Webhook{
			Addr: DefaultWebhookAddr,
			Path: "/webhooks/resend",
		},
		Sync: Sync{
			Interval:          Duration(60 * time.Second),
			RequestsPerSecond: 4,
			BackfillPageSize:  100,
			MaxMessageBytes:   64 << 20,
		},
		LogLevel:  "info",
		LogFormat: "text",
	}
}

// DataDir returns the data directory: $FERRY_DATA_DIR if set, else the
// platform application-support location.
func DataDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv(EnvDataDir)); d != "" {
		return filepath.Clean(d), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: locate home directory: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "ferry"), nil
	case "windows":
		if d := os.Getenv("LocalAppData"); d != "" {
			return filepath.Join(d, "ferry"), nil
		}
		return filepath.Join(home, "AppData", "Local", "ferry"), nil
	default:
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			return filepath.Join(d, "ferry"), nil
		}
		return filepath.Join(home, ".local", "share", "ferry"), nil
	}
}

// Load reads the configuration from dir. An empty dir resolves via DataDir.
// A missing config.json is not an error: defaults are returned.
func Load(dir string) (Config, error) {
	var err error
	if dir == "" {
		if dir, err = DataDir(); err != nil {
			return Config{}, err
		}
	}
	cfg := Default()
	cfg.Dir = dir

	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cfg, cfg.Validate()
	case err != nil:
		return Config{}, fmt.Errorf("config: read: %w", err)
	}

	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", filepath.Join(dir, "config.json"), err)
	}
	cfg.Dir = dir
	return cfg, cfg.Validate()
}

// Save writes the configuration back to config.json.
func (c Config) Save() error {
	if c.Dir == "" {
		return errors.New("config: no data directory")
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(c.Dir, "config.json"), append(data, '\n'), 0o600)
}

// Validate rejects settings that would misbehave at runtime.
func (c Config) Validate() error {
	if c.Sync.RequestsPerSecond <= 0 || c.Sync.RequestsPerSecond > 10 {
		return fmt.Errorf("config: requests_per_second must be in (0,10], got %g", c.Sync.RequestsPerSecond)
	}
	if c.Sync.BackfillPageSize <= 0 || c.Sync.BackfillPageSize > 100 {
		return fmt.Errorf("config: backfill_page_size must be in (0,100], got %d", c.Sync.BackfillPageSize)
	}
	if c.Sync.MaxMessageBytes <= 0 {
		return errors.New("config: max_message_bytes must be positive")
	}
	if c.Sync.Interval < 0 {
		return errors.New("config: sync interval must not be negative")
	}
	if c.Webhook.Addr != "" && c.Webhook.Path == "" {
		return errors.New("config: webhook path must be set when webhook addr is")
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return errors.New("config: tls cert_file and key_file must be set together")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: unknown log_level %q", c.LogLevel)
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("config: unknown log_format %q", c.LogFormat)
	}
	return nil
}

// Path joins elements onto the data directory.
func (c Config) Path(elem ...string) string {
	return filepath.Join(append([]string{c.Dir}, elem...)...)
}

// DBPath is the SQLite database file.
func (c Config) DBPath() string { return c.Path("ferry.db") }

// BlobDir is the root of the content-addressed message store.
func (c Config) BlobDir() string { return c.Path("blobs") }

// ControlSocket is the Unix socket the CLI uses to reach a running daemon.
func (c Config) ControlSocket() string { return c.Path(DefaultControlSock) }

// EnsureDirs creates the data directory tree with owner-only permissions.
func (c Config) EnsureDirs() error {
	for _, d := range []string{c.Dir, c.BlobDir(), c.Path("tls")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("config: create %s: %w", d, err)
		}
	}
	return nil
}

// IsLoopback reports whether addr binds only to the local machine.
func IsLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return strings.HasPrefix(host, "127.")
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
