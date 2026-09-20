package service

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// launchd runs Ferry as a per-user LaunchAgent.
//
// A LaunchAgent, not a LaunchDaemon: agents run as the logged-in user, which
// is what Ferry needs: its data directory is under the user's home and its
// secrets are in the user's login Keychain, which a root daemon could not
// unlock.
type launchd struct{}

var _ Manager = (*launchd)(nil)

// Name implements Manager.
func (launchd) Name() string { return "launchd" }

// UnitPath implements Manager.
func (launchd) UnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// serviceTarget is how modern launchctl addresses a per-user service.
func serviceTarget() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), Label)
}

func domainTarget() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// Install writes the LaunchAgent and loads it.
func (l launchd) Install(ctx context.Context, cfg Config) error {
	path, err := l.UnitPath()
	if err != nil {
		return err
	}

	// Replacing a loaded agent needs it unloaded first, or launchctl keeps
	// running the old definition until the next login.
	if _, err := os.Stat(path); err == nil {
		_ = l.bootout(ctx)
	}

	if err := writeUnit(path, l.plist(cfg)); err != nil {
		return err
	}
	_ = run(ctx, "launchctl", "enable", serviceTarget())
	if err := run(ctx, "launchctl", "bootstrap", domainTarget(), path); err != nil {
		// bootstrap fails if the service is somehow still registered; fall
		// back to the older verb, which is tolerant of that.
		if err2 := run(ctx, "launchctl", "load", "-w", path); err2 != nil {
			return err
		}
	}
	return nil
}

// Uninstall stops the service and removes the plist.
func (l launchd) Uninstall(ctx context.Context) error {
	path, err := l.UnitPath()
	if err != nil {
		return err
	}
	_ = l.bootout(ctx)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("service: remove %s: %w", path, err)
	}
	return nil
}

func (l launchd) bootout(ctx context.Context) error {
	if err := run(ctx, "launchctl", "bootout", serviceTarget()); err == nil {
		return nil
	}
	path, err := l.UnitPath()
	if err != nil {
		return err
	}
	return run(ctx, "launchctl", "unload", "-w", path)
}

// Start loads the agent, which RunAtLoad then starts.
func (l launchd) Start(ctx context.Context) error {
	path, err := l.requireInstalled()
	if err != nil {
		return err
	}
	// A previous Stop left the agent booted out, so the normal path is to
	// bootstrap it back in. Clear any lingering disable first, or launchd
	// accepts the bootstrap and then refuses to run it.
	_ = run(ctx, "launchctl", "enable", serviceTarget())
	if err := run(ctx, "launchctl", "bootstrap", domainTarget(), path); err == nil {
		return nil
	}
	// Already loaded: just make sure it is up.
	return run(ctx, "launchctl", "kickstart", serviceTarget())
}

// Stop unloads the agent.
//
// Signalling it is not enough: the agent is registered with KeepAlive, so
// launchd restarts it within seconds of any kill. Booting it out of the
// domain is what actually stops it, and it stays stopped until Start
// bootstraps it back in.
func (l launchd) Stop(ctx context.Context) error {
	if _, err := l.requireInstalled(); err != nil {
		return err
	}
	return l.bootout(ctx)
}

// Restart unloads and reloads the agent.
//
// `kickstart -k` would be enough to bounce the process, but launchd would
// reuse the job exactly as it was loaded, so an edited plist would be
// ignored and the restart would silently run the old definition. Booting out
// and back in is what makes "restart" mean "re-read everything".
func (l launchd) Restart(ctx context.Context) error {
	path, err := l.requireInstalled()
	if err != nil {
		return err
	}
	_ = l.bootout(ctx)
	_ = run(ctx, "launchctl", "enable", serviceTarget())
	if err := run(ctx, "launchctl", "bootstrap", domainTarget(), path); err != nil {
		// Already back up, or never fully out: make sure it is running.
		return run(ctx, "launchctl", "kickstart", serviceTarget())
	}
	return nil
}

// requireInstalled returns the path of the installed agent, or an error
// telling the user how to install it.
func (l launchd) requireInstalled() (string, error) {
	path, err := l.UnitPath()
	if err != nil {
		return "", err
	}
	if !installed(path) {
		return "", errors.New("service: not installed; run `ferry service install` first")
	}
	return path, nil
}

// Status implements Manager.
func (l launchd) Status(ctx context.Context) (Status, error) {
	path, err := l.UnitPath()
	if err != nil {
		return Status{}, err
	}
	st := Status{Manager: "launchd", UnitPath: path}
	if !installed(path) {
		return st, nil
	}
	st.Installed = true

	out, ok := probe(ctx, "launchctl", "print", serviceTarget())
	if !ok {
		// Not bootstrapped into the domain: installed but not loaded.
		st.Detail = "installed but not loaded; run `ferry service start`"
		return st, nil
	}

	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "pid = "):
			if pid, err := strconv.Atoi(strings.TrimPrefix(trimmed, "pid = ")); err == nil && pid > 0 {
				st.PID = pid
				st.Running = true
			}
		case strings.HasPrefix(trimmed, "last exit code = "):
			code := strings.TrimPrefix(trimmed, "last exit code = ")
			if code != "0" && code != "(never exited)" {
				st.Detail = "last exit code " + code
			}
		}
	}
	return st, nil
}

// plist renders the LaunchAgent. It is built with encoding/xml rather than
// string concatenation so a path containing an ampersand cannot produce a
// plist launchd silently refuses to parse.
func (launchd) plist(cfg Config) string {
	args := []string{cfg.Executable}
	if cfg.DataDir != "" {
		args = append(args, "--data-dir", cfg.DataDir)
	}
	args = append(args, "serve")

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")

	writeKey := func(k string) {
		b.WriteString("\t<key>" + escapeXML(k) + "</key>\n")
	}
	writeKey("Label")
	b.WriteString("\t<string>" + escapeXML(Label) + "</string>\n")

	writeKey("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range args {
		b.WriteString("\t\t<string>" + escapeXML(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")

	writeKey("RunAtLoad")
	b.WriteString("\t<true/>\n")
	writeKey("KeepAlive")
	b.WriteString("\t<true/>\n")

	if cfg.LogPath != "" {
		writeKey("StandardOutPath")
		b.WriteString("\t<string>" + escapeXML(cfg.LogPath) + "</string>\n")
		writeKey("StandardErrorPath")
		b.WriteString("\t<string>" + escapeXML(cfg.LogPath) + "</string>\n")
	}

	// Throttle restarts: a daemon that cannot bind its port should not spin.
	writeKey("ThrottleInterval")
	b.WriteString("\t<integer>10</integer>\n")

	// launchd gives an agent a minimal PATH; Ferry needs none of it, but the
	// `security` command that `ferry trust` shells out to lives in /usr/bin.
	writeKey("EnvironmentVariables")
	b.WriteString("\t<dict>\n\t\t<key>PATH</key>\n\t\t<string>/usr/bin:/bin:/usr/sbin:/sbin</string>\n\t</dict>\n")

	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func escapeXML(s string) string { return xmlEscaper.Replace(s) }
