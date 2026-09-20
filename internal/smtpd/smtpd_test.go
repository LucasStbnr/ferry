package smtpd_test

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/LucasStbnr/ferry/internal/account"
	"github.com/LucasStbnr/ferry/internal/resend"
	"github.com/LucasStbnr/ferry/internal/secrets"
	"github.com/LucasStbnr/ferry/internal/smtpd"
	"github.com/LucasStbnr/ferry/internal/store"
	"github.com/LucasStbnr/ferry/internal/testutil/fakeresend"
	"github.com/LucasStbnr/ferry/internal/tlsutil"
)

const (
	appPassword = "test-pass-word-0001"
	apiKey      = "re_test_key"
)

type fixture struct {
	api  *fakeresend.Server
	db   *store.DB
	addr string
	tls  *tls.Config
}

func newFixture(t *testing.T, domains ...string) *fixture {
	t.Helper()
	ctx := context.Background()

	api := fakeresend.New(apiKey)
	t.Cleanup(api.Close)

	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "ferry.db"), filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	hash, err := account.HashPassword(appPassword)
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.CreateAccount(ctx, "acct", "hello@mysite.test", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Account(a).EnsureDefaultMailboxes(ctx); err != nil {
		t.Fatal(err)
	}
	if len(domains) == 0 {
		domains = []string{"mysite.test"}
	}
	if err := db.SetDomains(ctx, "acct", domains); err != nil {
		t.Fatal(err)
	}

	sec := &secrets.Memory{}
	if err := sec.Set(secrets.APIKey("acct"), apiKey); err != nil {
		t.Fatal(err)
	}
	mgr := account.NewManager(db, sec, nil)
	mgr.NewClient = func(key string) *resend.Client {
		return resend.New(key, resend.WithBaseURL(api.URL), resend.WithRate(1000))
	}

	bundle, err := tlsutil.EnsureBundle(filepath.Join(dir, "tls"), []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := bundle.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}

	srv, err := smtpd.New(smtpd.Options{
		Addr:      "127.0.0.1:0",
		TLSConfig: serverTLS,
		DB:        db,
		Auth:      mgr,
		Clients:   mgr,
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, serverTLS)
	go srv.Serve(tlsLn)
	t.Cleanup(func() { srv.Close(); tlsLn.Close() })

	clientTLS, err := bundle.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{api: api, db: db, addr: ln.Addr().String(), tls: clientTLS}
}

func (f *fixture) dial(t *testing.T, auth bool) *smtp.Client {
	t.Helper()
	c, err := smtp.DialTLS(f.addr, f.tls)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	if auth {
		if err := c.Auth(sasl.NewPlainClient("", "acct", appPassword)); err != nil {
			t.Fatalf("auth: %v", err)
		}
	}
	return c
}

func message(from, to, subject, body string) string {
	return "From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-Id: <" + subject + "@mysite.test>\r\n" +
		"Date: Wed, 04 Mar 2026 09:30:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" + body + "\r\n"
}

func (f *fixture) send(t *testing.T, from string, to []string, msg string) error {
	t.Helper()
	c := f.dial(t, true)
	return c.SendMail(from, to, strings.NewReader(msg))
}

func (f *fixture) sentMessages(t *testing.T) []store.Message {
	t.Helper()
	ctx := context.Background()
	a, err := f.db.AccountByName(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	as := f.db.Account(a)
	mb, err := as.Mailbox(ctx, store.Sent)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := as.Messages(ctx, mb.ID)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestSendDeliversToResendAndFilesInSent(t *testing.T) {
	f := newFixture(t)
	msg := message("Lucas <hello@mysite.test>", "friend@example.test", "hello", "hi there")

	if err := f.send(t, "hello@mysite.test", []string{"friend@example.test"}, msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	sends := f.api.Sends()
	if len(sends) != 1 {
		t.Fatalf("%d messages reached Resend, want 1", len(sends))
	}
	got := sends[0]
	if !strings.Contains(got.From, "hello@mysite.test") {
		t.Errorf("From = %q", got.From)
	}
	if len(got.To) != 1 || !strings.Contains(got.To[0], "friend@example.test") {
		t.Errorf("To = %v", got.To)
	}
	if got.Subject != "hello" {
		t.Errorf("Subject = %q", got.Subject)
	}
	if !strings.Contains(got.Text, "hi there") {
		t.Errorf("Text = %q", got.Text)
	}

	// The copy in Sent must be what the user composed, byte for byte.
	sent := f.sentMessages(t)
	if len(sent) != 1 {
		t.Fatalf("%d messages in Sent, want 1", len(sent))
	}
	if sent[0].ResendID == "" {
		t.Error("the Sent copy was not linked to its Resend id, so sync would duplicate it")
	}
	a, _ := f.db.AccountByName(context.Background(), "acct")
	raw, err := f.db.Account(a).ReadBlob(sent[0].BlobHash)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != msg {
		t.Errorf("the Sent copy was rebuilt rather than stored verbatim")
	}
	seen := false
	for _, fl := range sent[0].Flags {
		if fl == `\Seen` {
			seen = true
		}
	}
	if !seen {
		t.Error("a message the user just sent should not be unread")
	}
}

func TestSendRequiresAuthentication(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t, false)
	err := c.SendMail("hello@mysite.test", []string{"friend@example.test"},
		strings.NewReader(message("hello@mysite.test", "friend@example.test", "x", "y")))
	if err == nil {
		t.Fatal("an unauthenticated submission was accepted")
	}
}

func TestAuthLoginMechanism(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t, false)
	// Apple Mail offers LOGIN for submission, so it has to work.
	if err := c.Auth(sasl.NewLoginClient("acct", appPassword)); err != nil {
		t.Fatalf("AUTH LOGIN: %v", err)
	}
	if err := c.SendMail("hello@mysite.test", []string{"friend@example.test"},
		strings.NewReader(message("hello@mysite.test", "friend@example.test", "viaLogin", "body"))); err != nil {
		t.Fatalf("send after AUTH LOGIN: %v", err)
	}
	if len(f.api.Sends()) != 1 {
		t.Fatal("the message did not reach Resend")
	}
}

func TestBadPasswordIsRejected(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t, false)
	if err := c.Auth(sasl.NewPlainClient("", "acct", "not-the-password")); err == nil {
		t.Fatal("a wrong password was accepted")
	}
}

func TestSendFromAnUnverifiedDomainIsRefused(t *testing.T) {
	f := newFixture(t) // only mysite.test is verified
	err := f.send(t, "someone@notmine.test", []string{"friend@example.test"},
		message("someone@notmine.test", "friend@example.test", "spoof", "body"))
	if err == nil {
		t.Fatal("sending from an unverified domain was accepted")
	}
	var smtpErr *smtp.SMTPError
	if !asSMTPError(err, &smtpErr) {
		t.Fatalf("error is %T, want an SMTP error: %v", err, err)
	}
	if smtpErr.Code/100 != 5 {
		t.Errorf("code = %d, want a permanent 5xx so Mail stops retrying", smtpErr.Code)
	}
	if !strings.Contains(smtpErr.Message, "notmine.test") {
		t.Errorf("message = %q, should name the offending domain", smtpErr.Message)
	}
	if len(f.api.Sends()) != 0 {
		t.Error("the message reached Resend despite the refusal")
	}
}

func TestSpoofedHeaderFromIsRefused(t *testing.T) {
	f := newFixture(t)
	// The envelope is a domain the account owns, but the visible From is not.
	err := f.send(t, "hello@mysite.test", []string{"friend@example.test"},
		message("ceo@bank.test", "friend@example.test", "spoof", "wire me money"))
	if err == nil {
		t.Fatal("a message whose header From is an unverified domain was accepted")
	}
	if len(f.api.Sends()) != 0 {
		t.Error("the spoofed message reached Resend")
	}
}

func TestBccFromEnvelopeIsCarried(t *testing.T) {
	f := newFixture(t)
	// Mail strips Bcc from the message and lists it only in RCPT TO.
	msg := message("hello@mysite.test", "friend@example.test", "party", "you are invited")
	if err := f.send(t, "hello@mysite.test",
		[]string{"friend@example.test", "secret@example.test"}, msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := f.api.Sends()[0]
	if len(got.Bcc) != 1 || got.Bcc[0] != "secret@example.test" {
		t.Fatalf("Bcc = %v, want [secret@example.test]", got.Bcc)
	}
	if len(got.To) != 1 {
		t.Fatalf("To = %v, the Bcc recipient must not be visible", got.To)
	}
}

func TestThreadingHeadersSurvive(t *testing.T) {
	f := newFixture(t)
	msg := "From: hello@mysite.test\r\nTo: friend@example.test\r\n" +
		"Subject: Re: hello\r\nMessage-Id: <reply@mysite.test>\r\n" +
		"In-Reply-To: <original@example.test>\r\n" +
		"References: <first@example.test> <original@example.test>\r\n\r\nreplying\r\n"
	if err := f.send(t, "hello@mysite.test", []string{"friend@example.test"}, msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	h := f.api.Sends()[0].Headers
	if h["In-Reply-To"] != "<original@example.test>" {
		t.Errorf("In-Reply-To = %q; without it the reply starts a new thread", h["In-Reply-To"])
	}
	if !strings.Contains(h["References"], "<first@example.test>") {
		t.Errorf("References = %q", h["References"])
	}
}

func TestAttachmentIsBase64Encoded(t *testing.T) {
	f := newFixture(t)
	msg := "From: hello@mysite.test\r\nTo: friend@example.test\r\nSubject: invoice\r\n" +
		"Message-Id: <inv@mysite.test>\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\nMime-Version: 1.0\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
		"--B\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("PDF-CONTENT")) + "\r\n--B--\r\n"

	if err := f.send(t, "hello@mysite.test", []string{"friend@example.test"}, msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := f.api.Sends()[0]
	if len(got.Attachments) != 1 {
		t.Fatalf("%d attachments reached Resend, want 1", len(got.Attachments))
	}
	if got.Attachments[0].Filename != "invoice.pdf" {
		t.Errorf("filename = %q", got.Attachments[0].Filename)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Attachments[0].Content)
	if err != nil {
		t.Fatalf("attachment content is not base64: %v", err)
	}
	if string(decoded) != "PDF-CONTENT" {
		t.Errorf("attachment content = %q", decoded)
	}
}

func TestSignedMessageIsRefusedLoudly(t *testing.T) {
	f := newFixture(t)
	msg := "From: hello@mysite.test\r\nTo: friend@example.test\r\nSubject: signed\r\n" +
		"Content-Type: multipart/signed; boundary=\"S\"; protocol=\"application/pkcs7-signature\"\r\n\r\n" +
		"--S\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--S\r\nContent-Type: application/pkcs7-signature\r\n\r\nc2ln\r\n--S--\r\n"

	err := f.send(t, "hello@mysite.test", []string{"friend@example.test"}, msg)
	if err == nil {
		t.Fatal("an S/MIME message was accepted, which would have sent it unsigned")
	}
	if !strings.Contains(err.Error(), "S/MIME") {
		t.Errorf("error = %q, should say what is unsupported", err)
	}
	if len(f.api.Sends()) != 0 {
		t.Error("the message was sent with its signature stripped")
	}
}

func TestQuotaErrorIsPermanentAndExplains(t *testing.T) {
	f := newFixture(t)
	f.api.FailSend(429, "daily_quota_exceeded")

	err := f.send(t, "hello@mysite.test", []string{"friend@example.test"},
		message("hello@mysite.test", "friend@example.test", "over", "body"))
	if err == nil {
		t.Fatal("a quota failure was reported as success")
	}
	var smtpErr *smtp.SMTPError
	if !asSMTPError(err, &smtpErr) {
		t.Fatalf("error is %T: %v", err, err)
	}
	if smtpErr.Code/100 != 5 {
		t.Errorf("code = %d, want 5xx: retrying will not help until the quota resets", smtpErr.Code)
	}
	if !strings.Contains(strings.ToLower(smtpErr.Message), "quota") {
		t.Errorf("message = %q, should mention the quota", smtpErr.Message)
	}
	if len(f.sentMessages(t)) != 0 {
		t.Error("a message that was never sent was filed in Sent")
	}
}

func TestServerErrorIsTemporary(t *testing.T) {
	f := newFixture(t)
	f.api.FailSend(503, "service_unavailable")

	err := f.send(t, "hello@mysite.test", []string{"friend@example.test"},
		message("hello@mysite.test", "friend@example.test", "later", "body"))
	if err == nil {
		t.Fatal("an API outage was reported as success")
	}
	var smtpErr *smtp.SMTPError
	if !asSMTPError(err, &smtpErr) {
		t.Fatalf("error is %T: %v", err, err)
	}
	if smtpErr.Code/100 != 4 {
		t.Errorf("code = %d, want 4xx so Mail retries", smtpErr.Code)
	}
}

func TestSendIsIdempotent(t *testing.T) {
	f := newFixture(t)
	msg := message("hello@mysite.test", "friend@example.test", "once", "body")

	// The same message submitted twice, as Mail would after a timeout.
	for i := 0; i < 2; i++ {
		if err := f.send(t, "hello@mysite.test", []string{"friend@example.test"}, msg); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	// Resend deduplicates on the idempotency key, so only the first send counts.
	if n := len(f.api.Sends()); n != 1 {
		t.Fatalf("Resend recorded %d sends of the same message, want 1", n)
	}
}

func TestTooManyRecipientsIsRefused(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t, true)
	if err := c.Mail("hello@mysite.test", nil); err != nil {
		t.Fatal(err)
	}
	var lastErr error
	for i := 0; i <= smtpd.MaxRecipients; i++ {
		lastErr = c.Rcpt(strings.Repeat("a", i+1)+"@example.test", nil)
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatalf("more than %d recipients were accepted", smtpd.MaxRecipients)
	}
}

func asSMTPError(err error, target **smtp.SMTPError) bool {
	return errors.As(err, target)
}
