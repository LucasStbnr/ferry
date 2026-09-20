package service

import (
	"encoding/xml"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The unit files are what a supervisor parses, so the tests check that they
// are well formed and that a hostile path cannot break out of them.

func TestLaunchdPlistIsValid(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plutil is only available on macOS")
	}
	plist := launchd{}.plist(Config{
		Executable: "/opt/homebrew/bin/ferry",
		DataDir:    "/Users/someone/Library/Application Support/ferry",
		LogPath:    "/Users/someone/Library/Application Support/ferry/ferry.log",
	})

	// plutil decides whether launchd will accept this, so let it.
	cmd := exec.Command("plutil", "-lint", "-")
	cmd.Stdin = strings.NewReader(plist)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plutil rejected the LaunchAgent: %v\n%s\n---\n%s", err, out, plist)
	}
}

func TestLaunchdPlistContents(t *testing.T) {
	plist := launchd{}.plist(Config{
		Executable: "/usr/local/bin/ferry",
		DataDir:    "/data/ferry",
		LogPath:    "/data/ferry/ferry.log",
	})
	for _, want := range []string{
		"<string>io.github.lucasstbnr.ferry</string>",
		"<string>/usr/local/bin/ferry</string>",
		"<string>--data-dir</string>",
		"<string>/data/ferry</string>",
		"<string>serve</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>ThrottleInterval</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist is missing %q", want)
		}
	}
	// The order matters: --data-dir has to precede the subcommand, because it
	// is a persistent flag on the root command.
	dataIdx := strings.Index(plist, "--data-dir")
	serveIdx := strings.Index(plist, "<string>serve</string>")
	if dataIdx < 0 || serveIdx < 0 || dataIdx > serveIdx {
		t.Error("--data-dir must come before the serve subcommand")
	}
}

func TestLaunchdPlistEscapesPaths(t *testing.T) {
	// A home directory really can contain an ampersand, and an unescaped one
	// makes the plist unparseable, and launchd would silently never start.
	plist := launchd{}.plist(Config{
		Executable: `/Users/A&B/bin/ferry`,
		DataDir:    `/Users/A&B/<data>`,
		LogPath:    `/Users/A&B/"log".txt`,
	})
	if strings.Contains(plist, "/Users/A&B") {
		t.Error("an ampersand in a path was written unescaped")
	}
	if !strings.Contains(plist, "&amp;") {
		t.Errorf("expected escaped output:\n%s", plist)
	}
	var probe any
	if err := xml.Unmarshal([]byte(plist), &probe); err != nil {
		t.Fatalf("the plist is not well-formed XML: %v", err)
	}
}

func TestLaunchdPlistOmitsLogWhenUnset(t *testing.T) {
	plist := launchd{}.plist(Config{Executable: "/usr/local/bin/ferry", DataDir: "/data"})
	if strings.Contains(plist, "StandardOutPath") {
		t.Error("a log path key appeared without a log path")
	}
}

func TestSystemdUnitContents(t *testing.T) {
	unit := systemd{}.unit(Config{
		Executable: "/usr/local/bin/ferry",
		DataDir:    "/home/someone/.local/share/ferry",
	})
	for _, want := range []string{
		"ExecStart=/usr/local/bin/ferry --data-dir /home/someone/.local/share/ferry serve",
		"Restart=always",
		"WantedBy=default.target",
		"ReadWritePaths=/home/someone/.local/share/ferry",
		// The sandbox is the point of using systemd rather than nohup.
		"NoNewPrivileges=true",
		"ProtectSystem=strict",
		"ProtectHome=read-only",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit is missing %q:\n%s", want, unit)
		}
	}
}

func TestSystemdUnitQuotesAwkwardPaths(t *testing.T) {
	unit := systemd{}.unit(Config{
		Executable: "/usr/local/bin/ferry",
		DataDir:    "/home/some one/ferry data",
	})
	// An unquoted space would make systemd read the rest as another argument.
	if !strings.Contains(unit, `"/home/some one/ferry data"`) {
		t.Errorf("a path containing spaces was not quoted:\n%s", unit)
	}
}

func TestQuoteArg(t *testing.T) {
	cases := map[string]string{
		"/plain/path":     "/plain/path",
		"/with space":     `"/with space"`,
		`/with"quote`:     `"/with\"quote"`,
		`/with\backslash`: `"/with\\backslash"`,
		"":                `""`,
		"/with$dollar":    `"/with$dollar"`,
	}
	for in, want := range cases {
		if got := quoteArg(in); got != want {
			t.Errorf("quoteArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultConfigUsesTheRunningBinary(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.Executable) {
		t.Errorf("executable = %q, want an absolute path", cfg.Executable)
	}
	if cfg.LogPath == "" || !strings.HasSuffix(cfg.LogPath, "ferry.log") {
		t.Errorf("log path = %q", cfg.LogPath)
	}
	if _, err := DefaultConfig(""); err == nil {
		t.Error("an empty data directory should be rejected")
	}
}

func TestNewReturnsAManagerOrSaysWhyNot(t *testing.T) {
	mgr, err := New()
	switch runtime.GOOS {
	case "darwin":
		if err != nil {
			t.Fatalf("macOS should always have launchd: %v", err)
		}
		if mgr.Name() != "launchd" {
			t.Errorf("manager = %q", mgr.Name())
		}
		path, err := mgr.UnitPath()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(path, "Library/LaunchAgents/"+Label+".plist") {
			t.Errorf("unit path = %q", path)
		}
	case "linux":
		// systemd may or may not be reachable in a container or over ssh;
		// either answer is correct, but it must be one of them.
		if err != nil && mgr != nil {
			t.Error("both a manager and an error were returned")
		}
	default:
		if err == nil {
			t.Error("an unsupported platform should say so")
		}
	}
}

// TestStatusOnACleanSystem checks the read-only path does not blow up when
// nothing is installed, which is the state every new user is in.
func TestStatusOnACleanSystem(t *testing.T) {
	mgr, err := New()
	if err != nil {
		t.Skip("no service manager on this platform")
	}
	st, err := mgr.Status(t.Context())
	if err != nil {
		t.Fatalf("Status on a system with nothing installed: %v", err)
	}
	if st.Manager == "" {
		t.Error("Status did not name the service manager")
	}
	if st.UnitPath == "" {
		t.Error("Status did not report where the definition would live")
	}
}
