package secrets

import (
	"os"
	"path/filepath"
)

// DockerSecretsDir is where Docker mounts secrets by convention.
const DockerSecretsDir = "/run/secrets"

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

	if kr := (Keyring{}); kr.Available() {
		chain.Readers = append(chain.Readers, kr)
		chain.Writer = kr
		return chain
	}

	f := &File{Path: filepath.Join(dataDir, "secrets.json")}
	chain.Readers = append(chain.Readers, f)
	chain.Writer = f
	return chain
}
