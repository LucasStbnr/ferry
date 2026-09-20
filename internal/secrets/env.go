package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvPrefix prefixes environment variables read by Env.
const EnvPrefix = "FERRY_SECRET_"

// Env reads secrets from the process environment. It is read-only, which is
// what a container wants: the orchestrator owns the value.
type Env struct{}

var _ Store = Env{}

// EnvVar returns the environment variable name for a key. Dots and dashes
// become underscores and the name is upper-cased, so the API key of account
// "mysite" is FERRY_SECRET_API_KEY_MYSITE.
func EnvVar(key string) string {
	r := strings.NewReplacer(".", "_", "-", "_")
	return EnvPrefix + strings.ToUpper(r.Replace(key))
}

// Get looks the key up in the environment.
func (Env) Get(key string) (string, error) {
	if v, ok := os.LookupEnv(EnvVar(key)); ok && v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, EnvVar(key))
}

// Set always fails: the environment is not writable by Ferry.
func (Env) Set(key, value string) error {
	return fmt.Errorf("secrets: cannot write %s to the environment", EnvVar(key))
}

// Delete always fails, for the same reason as Set.
func (Env) Delete(key string) error {
	return fmt.Errorf("secrets: cannot remove %s from the environment", EnvVar(key))
}

// Describe implements Store.
func (Env) Describe() string { return "environment (" + EnvPrefix + "*)" }

// Dir reads secrets from files in a directory, one file per key. This is the
// Docker secrets convention (/run/secrets/<name>). It is read-only.
type Dir struct{ Root string }

var _ Store = Dir{}

// Get reads <Root>/<key with dots as underscores>.
func (d Dir) Get(key string) (string, error) {
	name := strings.NewReplacer(".", "_", "-", "_").Replace(key)
	b, err := os.ReadFile(filepath.Join(d.Root, name))
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%w: %s is empty", ErrNotFound, name)
	}
	return v, nil
}

// Set always fails: mounted secrets are managed outside Ferry.
func (d Dir) Set(key, value string) error {
	return fmt.Errorf("secrets: %s is read-only", d.Root)
}

// Delete always fails, for the same reason as Set.
func (d Dir) Delete(key string) error {
	return fmt.Errorf("secrets: %s is read-only", d.Root)
}

// Describe implements Store.
func (d Dir) Describe() string { return "files in " + d.Root }
