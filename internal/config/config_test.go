package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LucasStbnr/ferry/internal/config"
)

func TestDefaultsAreLoopbackAndValid(t *testing.T) {
	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
	// A fresh install must not expose mail to the network.
	if !config.IsLoopback(cfg.IMAP.Addr) || !config.IsLoopback(cfg.SMTP.Addr) {
		t.Fatalf("default binds are not loopback: imap=%s smtp=%s", cfg.IMAP.Addr, cfg.SMTP.Addr)
	}
	if cfg.Webhook.Addr != "" {
		t.Error("the webhook receiver must be off by default")
	}
	// Half of Resend's 10/s team budget, leaving room for the user's website.
	if cfg.Sync.RequestsPerSecond > 5 {
		t.Errorf("default rate %g leaves no headroom for other API users", cfg.Sync.RequestsPerSecond)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("a missing config.json should not be an error: %v", err)
	}
	if cfg.Dir != dir || cfg.IMAP.Addr != config.DefaultIMAPAddr {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Dir = dir
	cfg.IMAP.Addr = "127.0.0.1:2993"
	cfg.Sync.Interval = config.Duration(90 * time.Second)
	cfg.Webhook.Addr = ":8443"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	// The file must not be world-readable: it names ports and paths.
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("config.json is mode %04o", info.Mode().Perm())
	}

	got, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.IMAP.Addr != "127.0.0.1:2993" {
		t.Errorf("imap addr = %q", got.IMAP.Addr)
	}
	if got.Sync.Interval.D() != 90*time.Second {
		t.Errorf("interval = %v", got.Sync.Interval)
	}
}

func TestDurationAcceptsStringsAndNumbers(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) config.Config {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(dir)
		if err != nil {
			t.Fatalf("load %s: %v", body, err)
		}
		return cfg
	}
	if got := write(`{"sync":{"interval":"2m"}}`).Sync.Interval.D(); got != 2*time.Minute {
		t.Errorf(`interval "2m" = %v`, got)
	}
	if got := write(`{"sync":{"interval":30}}`).Sync.Interval.D(); got != 30*time.Second {
		t.Errorf("interval 30 = %v, want 30s", got)
	}
	if got := write(`{"sync":{"interval":"60s"}}`).Sync.Interval.String(); got != "1m0s" {
		t.Errorf("interval string = %q", got)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	dir := t.TempDir()
	// A typo in a config file must be reported, not silently ignored: a
	// misspelled "imap" key would leave Ferry listening somewhere unexpected.
	body := `{"imap":{"addr":"127.0.0.1:1993"},"imapp":{"addr":"x"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(dir); err == nil {
		t.Fatal("an unknown configuration key was accepted")
	}
}

func TestValidateRejectsUnsafeSettings(t *testing.T) {
	cases := map[string]func(*config.Config){
		"rate above the team limit": func(c *config.Config) { c.Sync.RequestsPerSecond = 25 },
		"zero rate":                 func(c *config.Config) { c.Sync.RequestsPerSecond = 0 },
		"oversized page":            func(c *config.Config) { c.Sync.BackfillPageSize = 500 },
		"negative interval":         func(c *config.Config) { c.Sync.Interval = config.Duration(-time.Second) },
		"webhook without a path":    func(c *config.Config) { c.Webhook.Addr, c.Webhook.Path = ":8443", "" },
		"half a TLS pair":           func(c *config.Config) { c.TLS.CertFile = "/tmp/cert.pem" },
		"unknown log level":         func(c *config.Config) { c.LogLevel = "chatty" },
	}
	for name, mutate := range cases {
		cfg := config.Default()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:1993":  true,
		"localhost:1993":  true,
		"[::1]:1993":      true,
		"127.0.0.53:993":  true,
		"0.0.0.0:993":     false,
		":993":            false,
		"192.168.1.5:993": false,
		"[::]:993":        false,
	}
	for addr, want := range cases {
		if got := config.IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestDataDirHonoursEnvironment(t *testing.T) {
	t.Setenv(config.EnvDataDir, "/data")
	dir, err := config.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/data" {
		t.Fatalf("data dir = %q, want /data (the Docker image relies on this)", dir)
	}
}

func TestEnsureDirsCreatesPrivateTree(t *testing.T) {
	cfg := config.Default()
	cfg.Dir = filepath.Join(t.TempDir(), "ferry")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{cfg.Dir, cfg.BlobDir(), cfg.Path("tls")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s is mode %04o; it holds mail and private keys", dir, perm)
		}
	}
	if !strings.HasSuffix(cfg.DBPath(), "ferry.db") {
		t.Errorf("db path = %q", cfg.DBPath())
	}
}
