package secrets_test

import (
	"os"
	"testing"

	"github.com/LucasStbnr/ferry/internal/secrets"
)

// TestLiveKeychainRoundTrip touches the real OS credential store, so it only
// runs when asked for. It exists because a secret that stores without error
// and then cannot be read back is exactly the failure that took Ferry's API
// key out from under the daemon.
func TestLiveKeychainRoundTrip(t *testing.T) {
	if os.Getenv("FERRY_LIVE_KEYCHAIN") == "" {
		t.Skip("set FERRY_LIVE_KEYCHAIN=1 to exercise the real credential store")
	}
	dir := t.TempDir()
	s := secrets.Open(dir)
	t.Logf("store: %s", s.Describe())

	const key = "api-key.livetest"
	t.Cleanup(func() { _ = s.Delete(key) })

	if err := s.Set(key, "RE_FAKE_VALUE"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(key)
	if err != nil || got != "RE_FAKE_VALUE" {
		t.Fatalf("Get right after Set = %q, %v", got, err)
	}

	// A second Open is what a separate process does; the value must survive
	// it, including the availability probe Open runs.
	got, err = secrets.Open(dir).Get(key)
	if err != nil || got != "RE_FAKE_VALUE" {
		t.Fatalf("Get after reopening = %q, %v", got, err)
	}
}
