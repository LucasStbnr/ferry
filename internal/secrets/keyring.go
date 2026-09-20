package secrets

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// KeyringService is the service name under which Ferry's items appear in the
// macOS Keychain (and in libsecret / wincred elsewhere).
const KeyringService = "ferry"

// Keyring stores secrets in the OS credential store: the login Keychain on
// macOS, Secret Service on Linux, Credential Manager on Windows.
type Keyring struct {
	// Service overrides KeyringService. Tests set it to stay isolated.
	Service string
}

var _ Store = Keyring{}

func (k Keyring) service() string {
	if k.Service != "" {
		return k.Service
	}
	return KeyringService
}

// Get reads an item from the OS credential store.
func (k Keyring) Get(key string) (string, error) {
	v, err := keyring.Get(k.service(), key)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return "", fmt.Errorf("%w: %s", ErrNotFound, key)
	case err != nil:
		return "", fmt.Errorf("secrets: read %s from keyring: %w", key, err)
	}
	return v, nil
}

// Set writes an item, replacing any previous value.
func (k Keyring) Set(key, value string) error {
	if err := keyring.Set(k.service(), key, value); err != nil {
		return fmt.Errorf("secrets: write %s to keyring: %w", key, err)
	}
	return nil
}

// Delete removes an item. A missing item is not an error.
func (k Keyring) Delete(key string) error {
	err := keyring.Delete(k.service(), key)
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("secrets: delete %s from keyring: %w", key, err)
}

// Describe implements Store.
func (k Keyring) Describe() string { return "OS keyring (service " + k.service() + ")" }

// Available reports whether the OS credential store can be reached. On a
// headless Linux box without a Secret Service there is none, and Ferry falls
// back to an encrypted-at-rest-by-the-filesystem file.
func (k Keyring) Available() bool {
	const probe = "ferry-availability-probe"
	if err := keyring.Set(k.service(), probe, "1"); err != nil {
		return false
	}
	_ = keyring.Delete(k.service(), probe)
	return true
}
