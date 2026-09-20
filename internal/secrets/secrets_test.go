package secrets_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LucasStbnr/ferry/internal/secrets"
)

func TestFileStoreRoundTrip(t *testing.T) {
	f := &secrets.File{Path: filepath.Join(t.TempDir(), "secrets.json")}

	if _, err := f.Get("api-key.acct"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("missing key gave %v, want ErrNotFound", err)
	}
	if err := f.Set("api-key.acct", "re_secret"); err != nil {
		t.Fatal(err)
	}
	got, err := f.Get("api-key.acct")
	if err != nil || got != "re_secret" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if err := f.Delete("api-key.acct"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("api-key.acct"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	// Deleting twice is not an error.
	if err := f.Delete("api-key.acct"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestEnvStoreIsReadOnly(t *testing.T) {
	t.Setenv("FERRY_SECRET_API_KEY_ACCT", "re_from_env")

	var e secrets.Env
	got, err := e.Get(secrets.APIKey("acct"))
	if err != nil || got != "re_from_env" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if err := e.Set("api-key.acct", "x"); err == nil {
		t.Error("the environment should not be writable")
	}
	if _, err := e.Get(secrets.APIKey("missing")); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("missing env var gave %v", err)
	}
}

func TestEnvVarNaming(t *testing.T) {
	if got := secrets.EnvVar(secrets.APIKey("mysite")); got != "FERRY_SECRET_API_KEY_MYSITE" {
		t.Errorf("EnvVar = %q", got)
	}
	if got := secrets.EnvVar(secrets.WebhookSecret("my-site")); got != "FERRY_SECRET_WEBHOOK_SECRET_MY_SITE" {
		t.Errorf("EnvVar = %q", got)
	}
}

func TestChainPrefersFirstReaderAndWritesToWriter(t *testing.T) {
	t.Setenv("FERRY_SECRET_API_KEY_ACCT", "from-env")

	stored := &secrets.Memory{}
	if err := stored.Set(secrets.APIKey("acct"), "from-store"); err != nil {
		t.Fatal(err)
	}
	chain := &secrets.Chain{
		Readers: []secrets.Store{secrets.Env{}, stored},
		Writer:  stored,
	}

	// The environment wins, so a container can override stored state.
	got, err := chain.Get(secrets.APIKey("acct"))
	if err != nil || got != "from-env" {
		t.Fatalf("get = %q, %v", got, err)
	}
	// A key only in the store still resolves.
	if err := stored.Set(secrets.APIKey("other"), "only-stored"); err != nil {
		t.Fatal(err)
	}
	if got, _ := chain.Get(secrets.APIKey("other")); got != "only-stored" {
		t.Fatalf("get = %q", got)
	}
	// Writes land in the writable store.
	if err := chain.Set(secrets.APIKey("third"), "written"); err != nil {
		t.Fatal(err)
	}
	if got, _ := stored.Get(secrets.APIKey("third")); got != "written" {
		t.Fatalf("write did not reach the store: %q", got)
	}
	if _, err := chain.Get("nothing.here"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("missing key gave %v", err)
	}
}

func TestChainWithoutWriterRefusesWrites(t *testing.T) {
	chain := &secrets.Chain{Readers: []secrets.Store{secrets.Env{}}}
	if err := chain.Set("api-key.acct", "x"); err == nil {
		t.Fatal("a read-only chain accepted a write")
	}
}

func TestRedactKeepsSecretsOutOfOutput(t *testing.T) {
	const key = "re_abcdefghijklmnopqrstuvwxyz"
	got := secrets.Redact(key)
	if strings.Contains(got, "defghijklmnopqrstuv") {
		t.Fatalf("Redact leaked the secret: %q", got)
	}
	if !strings.HasPrefix(got, "re_") || !strings.HasSuffix(got, "wxyz") {
		t.Errorf("Redact = %q; it should stay recognisable", got)
	}
	if secrets.Redact("") != "(unset)" {
		t.Errorf("Redact(\"\") = %q", secrets.Redact(""))
	}
	if got := secrets.Redact("short"); strings.Contains(got, "short") {
		t.Errorf("Redact leaked a short secret: %q", got)
	}
}

func TestDirStore(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, "api_key_acct"), "  re_from_file\n"); err != nil {
		t.Fatal(err)
	}
	d := secrets.Dir{Root: dir}
	got, err := d.Get(secrets.APIKey("acct"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "re_from_file" {
		t.Errorf("get = %q; surrounding whitespace should be trimmed", got)
	}
	if _, err := d.Get(secrets.APIKey("missing")); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("missing file gave %v", err)
	}
	if err := d.Set("api-key.acct", "x"); err == nil {
		t.Error("a mounted secrets directory should be read-only")
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

// TestStoreOverrideForcesFile guards the isolation the test suite depends on.
//
// The OS credential store is machine-global and keyed by service name alone,
// so a process pointed at a throwaway data directory still reads and writes
// the real user's secrets. FERRY_SECRET_STORE=file is what stops that, and
// without it the end-to-end tests once overwrote and then deleted a real
// Resend API key.
func TestStoreOverrideForcesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(secrets.EnvStore, "file")

	s := secrets.Open(dir)
	if !strings.Contains(s.Describe(), "secrets.json") {
		t.Fatalf("store = %q, want the file store", s.Describe())
	}
	if err := s.Set("api-key.someone", "value"); err != nil {
		t.Fatal(err)
	}
	// It must land on disk, in the directory we chose, and nowhere else.
	if _, err := os.Stat(filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("the secret did not land in the data directory: %v", err)
	}
}

func TestStoreOverrideForcesKeyring(t *testing.T) {
	t.Setenv(secrets.EnvStore, "keyring")
	s := secrets.Open(t.TempDir())
	if !strings.Contains(s.Describe(), "keyring") {
		t.Fatalf("store = %q, want the OS keyring", s.Describe())
	}
}

func TestStoreDefaultIsAutomatic(t *testing.T) {
	t.Setenv(secrets.EnvStore, "")
	s := secrets.Open(t.TempDir())
	// Either backend is a correct answer; what matters is that it picked one
	// and reports it, so `ferry doctor` can tell the user where secrets live.
	if d := s.Describe(); !strings.Contains(d, "keyring") && !strings.Contains(d, "secrets.json") {
		t.Fatalf("store = %q", d)
	}
}
