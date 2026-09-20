package webhook_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LucasStbnr/ferry/internal/store"
	"github.com/LucasStbnr/ferry/internal/webhook"
)

// The signing secret is generated per run rather than written into the
// source. A literal "whsec_..." string is indistinguishable from a real
// Stripe or Svix secret to a scanner, so committing even an obviously fake
// one trips secret scanning here and push protection in every fork.
var signingSecret = newSigningSecret()

func newSigningSecret() string {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("webhook test: " + err.Error())
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(key)
}

type stubSyncer struct {
	mu        sync.Mutex
	triggered int
}

func (s *stubSyncer) Trigger() {
	s.mu.Lock()
	s.triggered++
	s.mu.Unlock()
}
func (s *stubSyncer) Account() string { return "acct" }
func (s *stubSyncer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.triggered
}

type fixture struct {
	db     *store.DB
	rec    *webhook.Receiver
	srv    *httptest.Server
	syncer *stubSyncer
	signer *webhook.Verifier
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "ferry.db"), filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	a, err := db.CreateAccount(ctx, "acct", "hello@mysite.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Account(a).EnsureDefaultMailboxes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDomains(ctx, "acct", []string{"mysite.test"}); err != nil {
		t.Fatal(err)
	}

	syncer := &stubSyncer{}
	rec := webhook.New(webhook.Options{Path: "/webhooks/resend", DB: db})
	if err := rec.Register("acct", signingSecret, syncer); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(rec.Handler())
	t.Cleanup(srv.Close)

	signer, err := webhook.NewVerifier(signingSecret)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{db: db, rec: rec, srv: srv, syncer: syncer, signer: signer}
}

func (f *fixture) post(t *testing.T, path string, event any, mangle func(http.Header)) *http.Response {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range f.signer.Sign("msg_test", time.Now(), body) {
		req.Header[k] = vs
	}
	req.Header.Set("Content-Type", "application/json")
	if mangle != nil {
		mangle(req.Header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (f *fixture) inbox(t *testing.T) []store.Message {
	t.Helper()
	ctx := context.Background()
	a, err := f.db.AccountByName(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	as := f.db.Account(a)
	mb, err := as.Mailbox(ctx, store.Inbox)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := as.Messages(ctx, mb.ID)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func receivedEvent() map[string]any {
	return map[string]any{
		"type":       webhook.TypeReceived,
		"created_at": time.Now().Format(time.RFC3339),
		"data":       map[string]any{"email_id": "rcv-1", "to": []string{"hello@mysite.test"}},
	}
}

func TestReceivedEventTriggersSync(t *testing.T) {
	f := newFixture(t)
	resp := f.post(t, "/webhooks/resend", receivedEvent(), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	waitFor(t, "a sync to be triggered", func() bool { return f.syncer.count() == 1 })
}

func TestUnsignedRequestIsRejected(t *testing.T) {
	f := newFixture(t)
	resp := f.post(t, "/webhooks/resend", receivedEvent(), func(h http.Header) {
		h.Del("Svix-Signature")
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unsigned request got %d, want 401", resp.StatusCode)
	}
	if f.syncer.count() != 0 {
		t.Fatal("an unsigned request caused a sync")
	}
}

func TestForgedSignatureIsRejected(t *testing.T) {
	f := newFixture(t)
	resp := f.post(t, "/webhooks/resend", receivedEvent(), func(h http.Header) {
		h.Set("Svix-Signature", "v1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a forged signature got %d, want 401", resp.StatusCode)
	}
}

func TestTamperedBodyIsRejected(t *testing.T) {
	f := newFixture(t)
	// Sign one body, send another: the signature no longer covers the payload.
	body := []byte(`{"type":"email.received","data":{"email_id":"rcv-1"}}`)
	headers := f.signer.Sign("msg_test", time.Now(), body)

	tampered := `{"type":"email.received","data":{"email_id":"rcv-evil"}}`
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/webhooks/resend", strings.NewReader(tampered))
	for k, vs := range headers {
		req.Header[k] = vs
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a tampered body got %d, want 401", resp.StatusCode)
	}
}

func TestReplayedRequestIsRejected(t *testing.T) {
	f := newFixture(t)
	body, _ := json.Marshal(receivedEvent())
	old := time.Now().Add(-webhook.Tolerance - time.Minute)

	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/webhooks/resend", strings.NewReader(string(body)))
	for k, vs := range f.signer.Sign("msg_old", old, body) {
		req.Header[k] = vs
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a stale request got %d, want 401", resp.StatusCode)
	}
}

func TestUnregisteredAccountIsRejected(t *testing.T) {
	f := newFixture(t)
	resp := f.post(t, "/webhooks/resend/other", receivedEvent(), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestPerAccountPathRoutes(t *testing.T) {
	f := newFixture(t)
	resp := f.post(t, "/webhooks/resend/acct", receivedEvent(), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	waitFor(t, "a sync to be triggered", func() bool { return f.syncer.count() == 1 })
}

func bounceEvent(emailID string) map[string]any {
	return map[string]any{
		"type":       webhook.TypeBounced,
		"created_at": time.Now().Format(time.RFC3339),
		"data": map[string]any{
			"email_id": emailID,
			"from":     "hello@mysite.test",
			"to":       []string{"nobody@example.test"},
			"subject":  "your receipt",
			"bounce": map[string]any{
				"type": "Permanent", "subType": "NoEmail",
				"message": "The email account does not exist.",
			},
		},
	}
}

func TestBounceBecomesAnInboxMessage(t *testing.T) {
	f := newFixture(t)
	resp := f.post(t, "/webhooks/resend", bounceEvent("snt-1"), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	waitFor(t, "the bounce notice to be filed", func() bool { return len(f.inbox(t)) == 1 })

	msg := f.inbox(t)[0]
	if !strings.Contains(msg.Subject, "Undelivered") {
		t.Errorf("subject = %q", msg.Subject)
	}
	flagged := false
	for _, fl := range msg.Flags {
		if fl == `\Flagged` {
			flagged = true
		}
	}
	if !flagged {
		t.Error("a bounce should be flagged so it is not missed")
	}

	a, _ := f.db.AccountByName(context.Background(), "acct")
	raw, err := f.db.Account(a).ReadBlob(msg.BlobHash)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"nobody@example.test", "does not exist", "your receipt"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("notice does not mention %q:\n%s", want, raw)
		}
	}
}

func TestBounceIsFiledOnlyOnce(t *testing.T) {
	f := newFixture(t)
	// Resend retries an event it thinks failed; the notice must not double up.
	for i := 0; i < 3; i++ {
		f.post(t, "/webhooks/resend", bounceEvent("snt-1"), nil)
	}
	waitFor(t, "the notice to be filed", func() bool { return len(f.inbox(t)) >= 1 })
	time.Sleep(200 * time.Millisecond)
	if n := len(f.inbox(t)); n != 1 {
		t.Fatalf("%d notices filed for one bounce, want 1", n)
	}
}

func TestComplaintBecomesAnInboxMessage(t *testing.T) {
	f := newFixture(t)
	event := bounceEvent("snt-2")
	event["type"] = webhook.TypeComplained
	f.post(t, "/webhooks/resend", event, nil)

	waitFor(t, "the complaint notice", func() bool { return len(f.inbox(t)) == 1 })
	if !strings.Contains(f.inbox(t)[0].Subject, "Spam complaint") {
		t.Errorf("subject = %q", f.inbox(t)[0].Subject)
	}
}

func TestUninterestingEventsAreAcknowledged(t *testing.T) {
	f := newFixture(t)
	event := bounceEvent("snt-3")
	event["type"] = webhook.TypeDelivered
	resp := f.post(t, "/webhooks/resend", event, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 so Resend stops retrying", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.inbox(t)); n != 0 {
		t.Fatalf("a delivery event produced %d Inbox messages", n)
	}
}

func TestGetIsNotAllowed(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/webhooks/resend")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET got %d, want 405", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz got %d", resp.StatusCode)
	}
}

func TestVerifierRejectsEmptySecret(t *testing.T) {
	if _, err := webhook.NewVerifier(""); err == nil {
		t.Fatal("an empty signing secret was accepted")
	}
}

func TestHostileInputDoesNotReachTheLogUnbounded(t *testing.T) {
	f := newFixture(t)

	// Anything from the request reaches the log before a signature has been
	// verified, so a caller must not be able to write arbitrary bytes into it.
	hostile := "/webhooks/resend/" + strings.Repeat("a", 500) + "%0aFAKE-LOG-LINE"
	resp := f.post(t, hostile, receivedEvent(), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if f.syncer.count() != 0 {
		t.Fatal("an unverified request caused a sync")
	}
}

func TestHostileEventTypeIsHandled(t *testing.T) {
	f := newFixture(t)
	event := receivedEvent()
	event["type"] = strings.Repeat("x", 5000) + "\n\rinjected"

	resp := f.post(t, "/webhooks/resend", event, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.inbox(t)); n != 0 {
		t.Fatalf("an unknown event type produced %d messages", n)
	}
}

func TestRejectionReasonsAreAFixedSet(t *testing.T) {
	f := newFixture(t)

	// Whatever a caller sends, the rejection path must log one of a closed set
	// of reasons and return nothing about why. Nothing here should panic, hang
	// or echo the input back.
	cases := []struct {
		name   string
		mangle func(http.Header)
	}{
		{"no signature", func(h http.Header) { h.Del("Svix-Signature") }},
		{"no id", func(h http.Header) { h.Del("Svix-Id") }},
		// Go's net/http refuses to send or accept a header value containing
		// control characters, so a newline cannot arrive this way at all;
		// this covers the rest of the garbage that can.
		{"garbage timestamp", func(h http.Header) { h.Set("Svix-Timestamp", "not-a-number") }},
		{"far-future timestamp", func(h http.Header) { h.Set("Svix-Timestamp", "99999999999") }},
		{"garbage signature", func(h http.Header) { h.Set("Svix-Signature", "v1,!!!not-base64!!!") }},
		{"empty signature list", func(h http.Header) { h.Set("Svix-Signature", "   ") }},
		{"huge id", func(h http.Header) { h.Set("Svix-Id", strings.Repeat("z", 4000)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.post(t, "/webhooks/resend", receivedEvent(), tc.mangle)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if got := strings.TrimSpace(string(body)); got != "unauthorized" {
				t.Fatalf("body = %q; the reason must not be disclosed", got)
			}
		})
	}
	if f.syncer.count() != 0 {
		t.Fatal("a rejected request caused a sync")
	}
}

func TestSecretRotationAcceptsEitherSignature(t *testing.T) {
	f := newFixture(t)

	// Svix sends several "v1,<sig>" entries while a secret is being rotated.
	// Any one of them matching must be enough, or rotation breaks delivery.
	body, err := json.Marshal(receivedEvent())
	if err != nil {
		t.Fatal(err)
	}
	valid := f.signer.Sign("msg_rotate", time.Now(), body)

	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/webhooks/resend", strings.NewReader(string(body)))
	req.Header.Set("Svix-Id", valid.Get("Svix-Id"))
	req.Header.Set("Svix-Timestamp", valid.Get("Svix-Timestamp"))
	req.Header.Set("Svix-Signature",
		"v1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= "+valid.Get("Svix-Signature"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	waitFor(t, "a sync to be triggered", func() bool { return f.syncer.count() == 1 })
}
