package secrets

import (
	"os"
	"path/filepath"
	"strings"
)

// DockerSecretsDir is where Docker mounts secrets by convention.
const DockerSecretsDir = "/run/secrets"

// EnvStore forces which backend Open writes to, overriding the automatic
// choice. Valid values are "auto" (the default), "file" and "keyring".
//
// This is not only a convenience. The OS credential store is machine-global
// and keyed by service name alone, so a process pointed at a throwaway data
// directory still reads and writes the real user's secrets. Anything that
// must not touch them (the test suite above all) sets this to "file".
const EnvStore = "FERRY_SECRET_STORE"

// Open picks a secret store for the current environment:
//
//   - the environment and mounted Docker secrets are always readable, and win
//     over stored values so a container can be configured without state;
//   - writes go to the OS keyring when one is reachable (the login Keychain on
//     macOS), otherwise to a 0600 file in the data directory.
//
// dataDir is where the file fallback lives.
func Open(dataDir string) Store {
	chain := &Chain{Readers: []Store{Env{}}}
	if fi, err := os.Stat(DockerSecretsDir); err == nil && fi.IsDir() {
		chain.Readers = append(chain.Readers, Dir{Root: DockerSecretsDir})
	}

	useFile := func() Store {
		f := &File{Path: filepath.Join(dataDir, "secrets.json")}
		chain.Readers = append(chain.Readers, f)
		chain.Writer = f
		return chain
	}
	useKeyring := func() Store {
		kr := Keyring{}
		chain.Readers = append(chain.Readers, kr)
		chain.Writer = kr
		return chain
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvStore))) {
	case "file":
		return useFile()
	case "keyring", "keychain":
		return useKeyring()
	}

	if kr := (Keyring{}); kr.Available() {
		return useKeyring()
	}
	return useFile()
}
