// Package service installs Ferry as a background service under the platform's
// own supervisor: launchd on macOS, systemd on Linux.
//
// Ferry manages this itself rather than leaning on `brew services`, because
// that only works for Homebrew formulae, and a formula is the wrong artifact
// for a pre-built binary, which is why Homebrew and GoReleaser both point at
// casks now. Doing it here means the service works the same however Ferry was
// installed: a cask, `go install`, a release archive, or a build from source.
//
// Everything is per-user. Ferry holds one person's mail and binds to loopback;
// a system-wide daemon would need root for no benefit.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Label is the service identifier, used for the launchd label and the systemd
// unit name.
const Label = "io.github.lucasstbnr.ferry"

// ErrUnsupported is returned on a platform with no supported supervisor.
var ErrUnsupported = errors.New("service: no supported service manager on this platform")

// Status describes what the supervisor knows about the service.
type Status struct {
	// Installed reports whether a unit or plist exists.
	Installed bool
	// Running reports whether the supervisor currently has it up.
	Running bool
	// PID is the process id when running and known.
	PID int
	// UnitPath is where the definition lives, for the user to inspect.
	UnitPath string
	// Manager names the supervisor: "launchd" or "systemd".
	Manager string
	// Detail is any extra line worth showing, such as a failure reason.
	Detail string
}

// Config describes the service to install.
type Config struct {
	// Executable is the absolute path to the ferry binary.
	Executable string
	// DataDir is passed through as --data-dir so the service uses the same
	// directory the CLI does, even when it is not the default.
	DataDir string
	// LogPath is where stdout and stderr go.
	LogPath string
}

// Manager installs and controls the service.
type Manager interface {
	// Install writes the unit and registers it with the supervisor.
	Install(ctx context.Context, cfg Config) error
	// Uninstall stops the service and removes the unit.
	Uninstall(ctx context.Context) error
	// Start, Stop and Restart control an installed service.
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	// Status reports the current state. A supervisor that cannot be queried
	// is a state, not an error: it is reported in Status.Detail.
	Status(ctx context.Context) (Status, error)
	// UnitPath is where the definition lives.
	UnitPath() (string, error)
	// Name identifies the supervisor.
	Name() string
}

// New returns the manager for this platform.
func New() (Manager, error) {
	switch runtime.GOOS {
	case "darwin":
		return &launchd{}, nil
	case "linux":
		if !hasSystemd() {
			return nil, fmt.Errorf("%w: systemd --user is not available", ErrUnsupported)
		}
		return &systemd{}, nil
	}
	return nil, ErrUnsupported
}

func hasSystemd() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	// A user bus is what `systemctl --user` needs; without one (a container,
	// or a bare ssh session) the unit would install and never start.
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		return false
	}
	return true
}

// DefaultConfig fills in the paths for the current installation.
func DefaultConfig(dataDir string) (Config, error) {
	exe, err := os.Executable()
	if err != nil {
		return Config{}, fmt.Errorf("service: locate the ferry binary: %w", err)
	}
	// Resolve symlinks: Homebrew installs a link in bin, and a supervisor
	// should point at the real file so an upgrade does not leave it dangling.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if dataDir == "" {
		return Config{}, errors.New("service: no data directory")
	}
	return Config{
		Executable: exe,
		DataDir:    dataDir,
		LogPath:    filepath.Join(dataDir, "ferry.log"),
	}, nil
}

// run executes a supervisor command and returns its combined output on error,
// because the message from launchctl or systemctl is the useful part.
func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if text == "" {
			return fmt.Errorf("service: %s %s: %w", name, strings.Join(args, " "), err)
		}
		return fmt.Errorf("service: %s %s: %w: %s", name, strings.Join(args, " "), err, text)
	}
	return nil
}

// probe runs a query command and reports whether it succeeded. Status uses it
// rather than run, because "the supervisor does not know about this unit" is
// an answer, not a failure to report upwards.
func probe(ctx context.Context, name string, args ...string) (string, bool) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err == nil
}

// installed reports whether a unit file is present. Its absence is the normal
// state before `ferry service install`, not something to report as an error.
func installed(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeUnit(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("service: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("service: write %s: %w", path, err)
	}
	return nil
}
