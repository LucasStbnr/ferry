package cli_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/LucasStbnr/ferry/internal/control"
	"github.com/LucasStbnr/ferry/internal/resend"
	"github.com/LucasStbnr/ferry/internal/testutil/fakeresend"
)

// These tests drive the real ferry binary end to end: add an account, start
// the daemon, then talk to it with real IMAP and SMTP clients. They are the
// closest thing to running a real mail client against it that can be
// automated.

const (
	apiKey = "re_test_key"
	// A name no real installation would use, so a test that escaped its
	// sandbox could not collide with someone's actual account.
	testAccount = "ferry-e2e-fixture"
)

type harness struct {
	t       *testing.T
	bin     string
	dataDir string
	api     *fakeresend.Server
	imap    string
	smtp    string

	password string
	daemon   *exec.Cmd
	logs     *strings.Builder
}

func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ferry")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/LucasStbnr/ferry/cmd/ferry")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ferry: %v\n%s", err, out)
	}
	return bin
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	api := fakeresend.New(apiKey)
	api.SetDomains(resend.Domain{
		ID: "dom-1", Name: "mysite.test", Status: "verified",
		Capabilities: resend.DomainCapabilities{Sending: "enabled", Receiving: "enabled"},
	})
	t.Cleanup(api.Close)

	// A real install creates its data directory 0700; t.TempDir gives 0755,
	// which doctor rightly warns about.
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	return &harness{
		t:       t,
		bin:     build(t),
		dataDir: dataDir,
		api:     api,
		imap:    freePort(t),
		smtp:    freePort(t),
		logs:    &strings.Builder{},
	}
}

// env is the environment every ferry subprocess runs with.
//
// FERRY_SECRET_STORE=file is not optional. The OS credential store is
// machine-global and keyed by service name alone, so without it these tests
// would overwrite and then delete the real user's Resend API key, which is
// exactly what happened once.
func (h *harness) env() []string {
	return append(os.Environ(),
		"FERRY_RESEND_BASE_URL="+h.api.URL,
		"FERRY_SECRET_STORE=file",
	)
}

// run executes a ferry subcommand and returns its combined output.
func (h *harness) run(args ...string) (string, error) {
	h.t.Helper()
	cmd := exec.Command(h.bin, append([]string{"--data-dir", h.dataDir}, args...)...)
	cmd.Env = h.env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	out, err := h.run(args...)
	if err != nil {
		h.t.Fatalf("ferry %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

var passwordRE = regexp.MustCompile(`App password\s+(\S+)`)

func (h *harness) addAccount(name string) {
	h.t.Helper()
	out := h.mustRun("account", "add", name, "--api-key", apiKey, "--address", "hello@mysite.test")
	m := passwordRE.FindStringSubmatch(out)
	if m == nil {
		h.t.Fatalf("could not find the app password in:\n%s", out)
	}
	h.password = m[1]
}

func (h *harness) startDaemon() {
	h.t.Helper()
	cmd := exec.Command(h.bin,
		"--data-dir", h.dataDir, "--log-level", "debug",
		"serve", "--imap-addr", h.imap, "--smtp-addr", h.smtp)
	cmd.Env = h.env()
	cmd.Stdout = h.logs
	cmd.Stderr = h.logs
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.daemon = cmd
	h.t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
		if h.t.Failed() {
			h.t.Logf("daemon log:\n%s", h.logs.String())
		}
	})

	// Wait for the control socket, which the daemon creates after binding.
	socket := filepath.Join(h.dataDir, control.SocketName)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if control.Available(h.t.Context(), socket) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("daemon did not start within 30s; log:\n%s", h.logs.String())
}

// clientTLS trusts the CA the daemon generated, exactly as a mail client does
// after `ferry trust`.
func (h *harness) clientTLS() *tls.Config {
	h.t.Helper()
	pem, err := os.ReadFile(filepath.Join(h.dataDir, "tls", "ca.crt"))
	if err != nil {
		h.t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		h.t.Fatal("CA certificate is not valid PEM")
	}
	return &tls.Config{RootCAs: pool, ServerName: "localhost"}
}

func (h *harness) imapClient(account string) *imapclient.Client {
	h.t.Helper()
	c, err := imapclient.DialTLS(h.imap, &imapclient.Options{TLSConfig: h.clientTLS()})
	if err != nil {
		h.t.Fatalf("imap dial: %v", err)
	}
	h.t.Cleanup(func() { c.Close() })
	if err := c.Login(account, h.password).Wait(); err != nil {
		h.t.Fatalf("imap login: %v", err)
	}
	return c
}

func (h *harness) addReceived(subject, body string) {
	m := &fakeresend.Mail{}
	m.From = "sender@example.test"
	m.To = resend.Addrs{"hello@mysite.test"}
	m.Subject = subject
	m.MessageID = "<" + subject + "@example.test>"
	m.Text = body
	m.RawMIME = []byte("From: sender@example.test\r\nTo: hello@mysite.test\r\n" +
		"Subject: " + subject + "\r\nMessage-Id: <" + subject + "@example.test>\r\n" +
		"Date: Wed, 04 Mar 2026 09:30:00 +0000\r\n\r\n" + body + "\r\n")
	h.api.AddReceived(m)
}

func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test builds the binary and starts a daemon")
	}
	h := newHarness(t)

	h.addReceived("welcome", "the quick brown fox jumps")
	h.addAccount(testAccount)
	h.startDaemon()

	// The account must sync without being asked.
	var c *imapclient.Client
	deadline := time.Now().Add(30 * time.Second)
	for {
		c = h.imapClient(testAccount)
		sel, err := c.Select("INBOX", nil).Wait()
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if sel.NumMessages > 0 {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatalf("mail never arrived in INBOX; daemon log:\n%s", h.logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Run("read a message", func(t *testing.T) {
		msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
			Envelope:    true,
			BodySection: []*imap.FetchItemBodySection{{Peek: true}},
		}).Collect()
		if err != nil {
			t.Fatal(err)
		}
		if msgs[0].Envelope.Subject != "welcome" {
			t.Errorf("subject = %q", msgs[0].Envelope.Subject)
		}
		body := msgs[0].FindBodySection(&imap.FetchItemBodySection{Peek: true})
		if !strings.Contains(string(body), "quick brown fox") {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("search", func(t *testing.T) {
		data, err := c.Search(&imap.SearchCriteria{Text: []string{"brown"}}, nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		if len(data.AllSeqNums()) != 1 {
			t.Errorf("search found %v, want one message", data.AllSeqNums())
		}
	})

	t.Run("send a reply", func(t *testing.T) {
		sc, err := smtp.DialTLS(h.smtp, h.clientTLS())
		if err != nil {
			t.Fatalf("smtp dial: %v", err)
		}
		defer sc.Close()
		if err := sc.Auth(sasl.NewPlainClient("", testAccount, h.password)); err != nil {
			t.Fatalf("smtp auth: %v", err)
		}
		msg := "From: hello@mysite.test\r\nTo: sender@example.test\r\n" +
			"Subject: Re: welcome\r\nMessage-Id: <reply@mysite.test>\r\n" +
			"In-Reply-To: <welcome@example.test>\r\n\r\nthanks for writing\r\n"
		if err := sc.SendMail("hello@mysite.test", []string{"sender@example.test"},
			strings.NewReader(msg)); err != nil {
			t.Fatalf("send: %v", err)
		}

		sends := h.api.Sends()
		if len(sends) != 1 {
			t.Fatalf("%d messages reached Resend, want 1", len(sends))
		}
		if sends[0].Subject != "Re: welcome" {
			t.Errorf("subject = %q", sends[0].Subject)
		}
		if sends[0].Headers["In-Reply-To"] != "<welcome@example.test>" {
			t.Errorf("threading header lost: %v", sends[0].Headers)
		}

		// The reply must appear in Sent straight away.
		sc2 := h.imapClient(testAccount)
		sel, err := sc2.Select("Sent", nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		if sel.NumMessages != 1 {
			t.Fatalf("Sent has %d messages, want 1", sel.NumMessages)
		}
	})

	t.Run("delete stays deleted", func(t *testing.T) {
		dc := h.imapClient(testAccount)
		if _, err := dc.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		if err := dc.Store(imap.SeqSetNum(1), &imap.StoreFlags{
			Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted},
		}, nil).Close(); err != nil {
			t.Fatal(err)
		}
		if err := dc.Expunge().Close(); err != nil {
			t.Fatal(err)
		}

		// Force a full re-walk: the message is still in Resend.
		if out, err := h.run("sync", "--backfill"); err != nil {
			t.Fatalf("sync: %v\n%s", err, out)
		}

		sel, err := dc.Select("INBOX", nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		if sel.NumMessages != 0 {
			t.Fatalf("the deleted message came back (%d in INBOX)", sel.NumMessages)
		}
	})

	t.Run("status reports the running daemon", func(t *testing.T) {
		out := h.mustRun("status", "--json")
		var st control.Status
		if err := json.Unmarshal([]byte(out), &st); err != nil {
			t.Fatalf("status is not JSON: %v\n%s", err, out)
		}
		if len(st.Accounts) != 1 || st.Accounts[0].Name != testAccount {
			t.Fatalf("status accounts = %+v", st.Accounts)
		}
		if st.IMAPAddr != h.imap {
			t.Errorf("imap addr = %q, want %q", st.IMAPAddr, h.imap)
		}
	})

	t.Run("doctor passes", func(t *testing.T) {
		out, err := h.run("doctor")
		if err != nil {
			t.Fatalf("doctor reported problems: %v\n%s", err, out)
		}
		if !strings.Contains(out, "[ok  ] Account "+testAccount) {
			t.Errorf("doctor output:\n%s", out)
		}
	})

	t.Run("mail profile is written", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ferry.mobileconfig")
		h.mustRun("mail-profile", testAccount, "-o", path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(h.imap)
		if !strings.Contains(string(data), "<integer>"+port+"</integer>") {
			t.Errorf("profile does not carry the IMAP port %s", port)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("profile is mode %04o; it may contain a credential", info.Mode().Perm())
		}
	})
}

func TestAccountLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test builds the binary")
	}
	h := newHarness(t)
	h.addAccount(testAccount)

	out := h.mustRun("account", "list")
	if !strings.Contains(out, testAccount) || !strings.Contains(out, "mysite.test") {
		t.Fatalf("account list:\n%s", out)
	}

	// A second account with the same name must be refused.
	if out, err := h.run("account", "add", testAccount, "--api-key", apiKey); err == nil {
		t.Fatalf("a duplicate account name was accepted:\n%s", out)
	}

	// An invalid key is rejected before anything is stored.
	if out, err := h.run("account", "add", "other", "--api-key", "re_wrong"); err == nil {
		t.Fatalf("an invalid API key was accepted:\n%s", out)
	}
	if out := h.mustRun("account", "list"); strings.Contains(out, "other") {
		t.Fatalf("a failed add left an account behind:\n%s", out)
	}

	old := h.password
	out = h.mustRun("account", "passwd", testAccount)
	if strings.Contains(out, old) {
		t.Error("the old password was printed again instead of a new one")
	}

	h.mustRun("account", "remove", testAccount, "--force")
	if out := h.mustRun("account", "list"); strings.Contains(out, testAccount) {
		t.Fatalf("account was not removed:\n%s", out)
	}
}

func TestVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test builds the binary")
	}
	h := newHarness(t)
	out := h.mustRun("version")
	if !strings.HasPrefix(out, "ferry ") {
		t.Fatalf("version output = %q", out)
	}
}

// TestMailProfilePerAccount covers the bug where adding a second account tore
// down the first.
//
// Every account used to go into one profile with a fixed identifier.
// Installing a profile whose identifier already exists replaces it, and macOS
// removes the old one first, taking its mail accounts with it. So generating
// the profile again after adding an account removed the original account from
// Mail and asked for every password afresh.
func TestMailProfilePerAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test builds the binary")
	}
	h := newHarness(t)
	h.addAccount(testAccount)
	h.addAccount("second-fixture")

	out := h.mustRun("mail-profile")
	if !strings.Contains(out, "ferry-"+testAccount+".mobileconfig") ||
		!strings.Contains(out, "ferry-second-fixture.mobileconfig") {
		t.Fatalf("expected one profile per account:\n%s", out)
	}

	ids := map[string]string{}
	for _, name := range []string{testAccount, "second-fixture"} {
		path := filepath.Join(h.dataDir, "ferry-"+name+".mobileconfig")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body := string(data)
		// The identifier must be unique per account, or installing one
		// replaces the other.
		want := "<string>io.github.lucasstbnr.ferry." + name + "</string>"
		if !strings.Contains(body, want) {
			t.Errorf("%s does not carry its own profile identifier", name)
		}
		// And it must describe only its own account.
		other := testAccount
		if name == testAccount {
			other = "second-fixture"
		}
		if strings.Contains(body, "io.github.lucasstbnr.ferry."+other) {
			t.Errorf("%s's profile also configures %s", name, other)
		}
		ids[name] = want
	}
	if ids[testAccount] == ids["second-fixture"] {
		t.Fatal("both profiles share an identifier")
	}

	// -o names one file, so it cannot stand in for several.
	if out, err := h.run("mail-profile", "-o", filepath.Join(t.TempDir(), "x.mobileconfig")); err == nil {
		t.Fatalf("-o with two accounts should be refused:\n%s", out)
	}
	// With one account named, it is fine.
	single := filepath.Join(t.TempDir(), "one.mobileconfig")
	h.mustRun("mail-profile", testAccount, "-o", single)
	if _, err := os.Stat(single); err != nil {
		t.Fatalf("naming one account with -o should work: %v", err)
	}
}
