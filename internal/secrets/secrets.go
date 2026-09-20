// Package secrets stores API keys and webhook signing secrets outside the
// database. On macOS they live in the login Keychain; in a container they come
// from the environment or a mounted secrets directory (Docker secrets).
//
// Nothing in this package ever logs or formats a secret value.
package secrets

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is returned when a key has no stored value.
var ErrNotFound = errors.New("secrets: not found")

// Store reads and writes named secrets.
type Store interface {
	// Get returns the value for key, or ErrNotFound.
	Get(key string) (string, error)
	// Set stores a value, replacing any previous one.
	Set(key, value string) error
	// Delete removes a key. Deleting a missing key is not an error.
	Delete(key string) error
	// Describe names the backend for `ferry doctor`, without revealing values.
	Describe() string
}

// Key names. Account-scoped secrets are namespaced by account name so two
// Resend accounts never collide.
const (
	kindAPIKey        = "api-key"
	kindWebhookSecret = "webhook-secret"
)

// APIKey is the key name holding an account's Resend API key.
func APIKey(account string) string { return kindAPIKey + "." + account }

// WebhookSecret is the key name holding an account's Svix signing secret.
func WebhookSecret(account string) string { return kindWebhookSecret + "." + account }

// Redact renders a secret for display: enough to recognise, not enough to use.
func Redact(v string) string {
	if v == "" {
		return "(unset)"
	}
	if len(v) <= 8 {
		return strings.Repeat("*", len(v))
	}
	return v[:3] + strings.Repeat("*", len(v)-7) + v[len(v)-4:]
}

// Chain reads from each store in order and writes to the first writable one.
// Ferry uses it so an environment variable can override the Keychain without
// the Keychain entry having to be removed.
type Chain struct {
	// Readers are consulted in order by Get.
	Readers []Store
	// Writer receives Set and Delete.
	Writer Store
}

var _ Store = (*Chain)(nil)

// Get returns the first value found among the readers.
func (c *Chain) Get(key string) (string, error) {
	for _, s := range c.Readers {
		v, err := s.Get(key)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, key)
}

// Set writes through to the writable store.
func (c *Chain) Set(key, value string) error {
	if c.Writer == nil {
		return errors.New("secrets: no writable secret store configured")
	}
	return c.Writer.Set(key, value)
}

// Delete removes the key from the writable store.
func (c *Chain) Delete(key string) error {
	if c.Writer == nil {
		return errors.New("secrets: no writable secret store configured")
	}
	return c.Writer.Delete(key)
}

// Describe lists the backends in use.
func (c *Chain) Describe() string {
	parts := make([]string, 0, len(c.Readers))
	for _, s := range c.Readers {
		parts = append(parts, s.Describe())
	}
	return strings.Join(parts, " then ")
}
