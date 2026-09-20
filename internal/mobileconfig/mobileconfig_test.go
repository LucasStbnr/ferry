package mobileconfig_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LucasStbnr/ferry/internal/mobileconfig"
	"github.com/LucasStbnr/ferry/internal/tlsutil"
)

func buildProfile(t *testing.T, password string) []byte {
	t.Helper()
	return buildProfileIn(t, t.TempDir(), password)
}

// buildProfileIn reuses a TLS directory, so two builds share one CA and any
// difference between them comes from the profile writer.
func buildProfileIn(t *testing.T, dir, password string) []byte {
	t.Helper()
	bundle, err := tlsutil.EnsureBundle(filepath.Join(dir, "tls"), []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := mobileconfig.Build(mobileconfig.Options{
		Identifier: "club.polygone.ferry",
		CACertPEM:  bundle.CACertPEM,
		Accounts: []mobileconfig.Account{{
			Name: "polygone", DisplayName: "Polygone", Address: "hello@polygone.club",
			Password: password,
			IMAPHost: "localhost", IMAPPort: 1993,
			SMTPHost: "localhost", SMTPPort: 1465,
		}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return profile
}

func TestProfileIsValidPlist(t *testing.T) {
	profile := buildProfile(t, "")
	if runtime.GOOS != "darwin" {
		t.Skip("plutil is only available on macOS")
	}
	// plutil is the authority on whether Mail's profile installer will read
	// this, so let it decide rather than trusting the writer.
	cmd := exec.Command("plutil", "-lint", "-")
	cmd.Stdin = strings.NewReader(string(profile))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plutil rejected the profile: %v\n%s\n---\n%s", err, out, profile)
	}
}

func TestProfileContainsAccountAndCA(t *testing.T) {
	got := string(buildProfile(t, ""))
	for _, want := range []string{
		"com.apple.mail.managed",
		"com.apple.security.root",
		"hello@polygone.club",
		"<integer>1993</integer>",
		"<integer>1465</integer>",
		"IncomingMailServerUseSSL",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("profile is missing %q", want)
		}
	}
}

func TestPasswordIsOmittedUnlessAsked(t *testing.T) {
	if strings.Contains(string(buildProfile(t, "")), "<key>IncomingPassword</key>") {
		t.Error("a password key appeared in a profile built without one")
	}
	with := string(buildProfile(t, "abcd-efgh-ijkl-mnop-qrst"))
	if !strings.Contains(with, "abcd-efgh-ijkl-mnop-qrst") {
		t.Error("the password was not embedded when asked for")
	}
}

func TestProfileIsDeterministic(t *testing.T) {
	// Reinstalling a regenerated profile should update the existing one, which
	// only works if the UUIDs are stable.
	dir := t.TempDir()
	a, b := buildProfileIn(t, dir, ""), buildProfileIn(t, dir, "")
	if string(a) != string(b) {
		t.Error("two builds of the same profile differ")
	}
}

func TestBuildRejectsEmptyAccountList(t *testing.T) {
	if _, err := mobileconfig.Build(mobileconfig.Options{}); err == nil {
		t.Fatal("a profile with no accounts should not build")
	}
}

func TestXMLIsEscaped(t *testing.T) {
	profile, err := mobileconfig.Build(mobileconfig.Options{
		Accounts: []mobileconfig.Account{{
			Name: "acct", DisplayName: `Tom & "Jerry" <test>`, Address: "a@b.test",
			IMAPHost: "localhost", IMAPPort: 1993, SMTPHost: "localhost", SMTPPort: 1465,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(profile), `Tom & "Jerry" <test>`) {
		t.Error("special characters were written into the XML unescaped")
	}
	if !strings.Contains(string(profile), "Tom &amp; &quot;Jerry&quot; &lt;test&gt;") {
		t.Errorf("escaping is wrong:\n%s", profile)
	}
}
