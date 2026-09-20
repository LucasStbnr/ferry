package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// systemd runs Ferry as a per-user systemd unit.
//
// `systemctl --user`, not a system unit: Ferry's data directory, its logs and
// its secrets all belong to one user, and a system unit would run as root with
// no way to reach them.
type systemd struct{}

var _ Manager = (*systemd)(nil)

const unitName = "ferry.service"

// Name implements Manager.
func (systemd) Name() string { return "systemd" }

// UnitPath implements Manager.
func (systemd) UnitPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", unitName), nil
}

// Install writes the unit, reloads systemd and enables it.
func (s systemd) Install(ctx context.Context, cfg Config) error {
	path, err := s.UnitPath()
	if err != nil {
		return err
	}
	if err := writeUnit(path, s.unit(cfg)); err != nil {
		return err
	}
	if err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	// --now starts it as well as enabling it at login.
	if err := run(ctx, "systemctl", "--user", "enable", "--now", unitName); err != nil {
		return err
	}
	// Without lingering the unit stops when the user logs out, which is not
	// what someone installing a mail bridge expects. It needs privileges, so
	// a failure is reported by Status rather than failing the install.
	_ = run(ctx, "loginctl", "enable-linger", currentUser())
	return nil
}

// Uninstall stops the service and removes the unit.
func (s systemd) Uninstall(ctx context.Context) error {
	path, err := s.UnitPath()
	if err != nil {
		return err
	}
	_ = run(ctx, "systemctl", "--user", "disable", "--now", unitName)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("service: remove %s: %w", path, err)
	}
	return run(ctx, "systemctl", "--user", "daemon-reload")
}

// Start implements Manager.
func (s systemd) Start(ctx context.Context) error {
	return run(ctx, "systemctl", "--user", "start", unitName)
}

// Stop implements Manager.
func (s systemd) Stop(ctx context.Context) error {
	return run(ctx, "systemctl", "--user", "stop", unitName)
}

// Restart implements Manager.
func (s systemd) Restart(ctx context.Context) error {
	return run(ctx, "systemctl", "--user", "restart", unitName)
}

// Status implements Manager.
func (s systemd) Status(ctx context.Context) (Status, error) {
	path, err := s.UnitPath()
	if err != nil {
		return Status{}, err
	}
	st := Status{Manager: "systemd", UnitPath: path}
	if !installed(path) {
		return st, nil
	}
	st.Installed = true

	// `show` is the machine-readable form; `status` is for humans and exits
	// non-zero when the unit is merely stopped.
	out, ok := probe(ctx, "systemctl", "--user", "show", unitName,
		"--property=ActiveState,SubState,MainPID,Result")
	if !ok {
		st.Detail = "installed, but systemd could not be queried"
		return st, nil
	}
	for line := range strings.SplitSeq(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "ActiveState":
			st.Running = value == "active"
		case "MainPID":
			if pid, err := strconv.Atoi(value); err == nil && pid > 0 {
				st.PID = pid
			}
		case "Result":
			if value != "" && value != "success" {
				st.Detail = "last result: " + value
			}
		}
	}
	return st, nil
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return strconv.Itoa(os.Getuid())
}

func (systemd) unit(cfg Config) string {
	execStart := cfg.Executable
	if cfg.DataDir != "" {
		execStart += " --data-dir " + quoteArg(cfg.DataDir)
	}
	execStart += " serve"

	return `[Unit]
Description=Ferry, an IMAP and SMTP bridge for Resend
Documentation=https://github.com/LucasStbnr/ferry
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + execStart + `
Restart=always
RestartSec=10s

# Ferry needs nothing but its own data directory and the network, so take
# away everything else. A bridge holding a mailbox is worth confining.
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=` + quoteArg(cfg.DataDir) + `
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=default.target
`
}

// quoteArg quotes a path for a systemd unit file if it needs it.
func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"'\\$%") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
