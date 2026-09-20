package mailsync_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LucasStbnr/ferry/internal/mailsync"
	"github.com/LucasStbnr/ferry/internal/resend"
	"github.com/LucasStbnr/ferry/internal/store"
	"github.com/LucasStbnr/ferry/internal/testutil/fakeresend"
)

const testKey = "re_test_key"

type harness struct {
	api  *fakeresend.Server
	db   *store.DB
	acct *store.AccountStore
	sync *mailsync.Syncer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	api := fakeresend.New(testKey)
	t.Cleanup(api.Close)

	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "ferry.db"), filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	a, err := db.CreateAccount(ctx, "acct", "hello@polygone.club", "hash")
	if err != nil {
		t.Fatal(err)
	}
	as := db.Account(a)
	if err := as.EnsureDefaultMailboxes(ctx); err != nil {
		t.Fatal(err)
	}

	client := resend.New(testKey,
		resend.WithBaseURL(api.URL),
		resend.WithRate(1000)) // tests must not wait on the rate limiter

	return &harness{
		api:  api,
		db:   db,
		acct: as,
		sync: mailsync.New(client, as, mailsync.Options{PageSize: 10}),
	}
}

func (h *harness) inboxUIDs(t *testing.T) []uint32 {
	t.Helper()
	ctx := context.Background()
	mb, err := h.acct.Mailbox(ctx, store.Inbox)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := h.acct.Messages(ctx, mb.ID)
	if err != nil {
		t.Fatal(err)
	}
	uids := make([]uint32, len(msgs))
	for i, m := range msgs {
		uids[i] = m.UID
	}
	return uids
}

func (h *harness) inbox(t *testing.T) []store.Message {
	t.Helper()
	mb, _ := h.acct.Mailbox(context.Background(), store.Inbox)
	msgs, err := h.acct.Messages(context.Background(), mb.ID)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func rawMail(subject, body string) []byte {
	return []byte("From: sender@example.test\r\n" +
		"To: hello@polygone.club\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-Id: <" + subject + "@example.test>\r\n" +
		"Date: Wed, 04 Mar 2026 09:30:00 +0000\r\n" +
		"\r\n" + body + "\r\n")
}

func addReceived(h *harness, subject, body string, withRaw bool) *fakeresend.Mail {
	m := &fakeresend.Mail{}
	m.From = "sender@example.test"
	m.To = resend.Addrs{"hello@polygone.club"}
	m.Subject = subject
	m.MessageID = "<" + subject + "@example.test>"
	m.Text = body
	m.HTML = "<p>" + body + "</p>"
	if withRaw {
		m.RawMIME = rawMail(subject, body)
	}
	return h.api.AddReceived(m)
}

func TestSyncStoresReceivedMailOldestFirst(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	addReceived(h, "first", "one", true)
	addReceived(h, "second", "two", true)
	addReceived(h, "third", "three", true)

	res, err := h.sync.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.ReceivedStored != 3 {
		t.Fatalf("stored %d received messages, want 3 (errors: %v)", res.ReceivedStored, res.Errors)
	}

	msgs := h.inbox(t)
	if len(msgs) != 3 {
		t.Fatalf("%d messages in INBOX, want 3", len(msgs))
	}
	// UIDs must follow arrival order, so Mail lists them chronologically.
	want := []string{"first", "second", "third"}
	for i, m := range msgs {
		if m.UID != uint32(i+1) {
			t.Errorf("message %d has uid %d, want %d", i, m.UID, i+1)
		}
		if m.Subject != want[i] {
			t.Errorf("uid %d subject = %q, want %q", m.UID, m.Subject, want[i])
		}
	}
	// Received mail arrives unread.
	for _, m := range msgs {
		for _, f := range m.Flags {
			if f == `\Seen` {
				t.Errorf("received message %q should not be marked read", m.Subject)
			}
		}
	}
}

func TestSyncIsIncrementalAndIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	addReceived(h, "first", "one", true)

	if _, err := h.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	before := h.api.RequestCount("GET /emails/receiving/")

	// A second sync with nothing new must not refetch bodies.
	res, err := h.sync.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReceivedStored != 0 {
		t.Fatalf("re-stored %d messages on an unchanged account", res.ReceivedStored)
	}
	if after := h.api.RequestCount("GET /emails/receiving/"); after != before {
		t.Fatalf("detail fetches went from %d to %d with nothing new", before, after)
	}
	if got := len(h.inbox(t)); got != 1 {
		t.Fatalf("%d messages after a repeated sync, want 1", got)
	}

	// New mail is picked up and appended after the existing UIDs.
	addReceived(h, "second", "two", true)
	if res, err = h.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if res.ReceivedStored != 1 {
		t.Fatalf("stored %d new messages, want 1", res.ReceivedStored)
	}
	if uids := h.inboxUIDs(t); len(uids) != 2 || uids[1] != 2 {
		t.Fatalf("uids = %v, want [1 2]", uids)
	}
}

func TestSyncNeverResurrectsDeletedMail(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	addReceived(h, "unwanted", "spam", true)

	if _, err := h.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	msgs := h.inbox(t)
	if len(msgs) != 1 {
		t.Fatalf("setup stored %d messages", len(msgs))
	}

	// Deleting locally writes a tombstone; Resend still has the message.
	if err := h.acct.Expunge(ctx, []int64{msgs[0].ID}); err != nil {
		t.Fatal(err)
	}

	// Force a full re-walk, as a reinstall or a cursor reset would.
	st, _ := h.acct.SyncState(ctx, store.KindReceived)
	st.NewestID, st.BackfillCursor, st.BackfillDone = "", "", false
	if err := h.acct.SaveSyncState(ctx, st); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := h.sync.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(h.inbox(t)); got != 0 {
		t.Fatalf("%d messages came back after a local delete", got)
	}
}

func TestSyncBackfillsFullHistoryAcrossPages(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	const total = 35 // more than the 10-row page size, so several backfill passes
	for i := 0; i < total; i++ {
		addReceived(h, "msg"+string(rune('a'+i%26))+string(rune('0'+i/26)), "body", true)
	}

	// Each Sync does one incremental pass plus at most one backfill page.
	for i := 0; i < 20; i++ {
		res, err := h.sync.Sync(ctx)
		if err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
		if !res.BackfillPending {
			break
		}
	}

	st, err := h.acct.SyncState(ctx, store.KindReceived)
	if err != nil {
		t.Fatal(err)
	}
	if !st.BackfillDone {
		t.Fatal("backfill never completed")
	}
	if got := len(h.inbox(t)); got != total {
		t.Fatalf("%d of %d messages backfilled", got, total)
	}
}

func TestSyncSynthesisesMIMEWhenRawIsMissing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	addReceived(h, "noraw", "body text", false) // no RawMIME: Resend has none

	if _, err := h.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	msgs := h.inbox(t)
	if len(msgs) != 1 {
		t.Fatalf("%d messages stored", len(msgs))
	}
	raw, err := h.acct.ReadBlob(msgs[0].BlobHash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "body text") {
		t.Fatalf("synthesised message lost its body:\n%s", raw)
	}
	if !strings.Contains(string(raw), "noraw") {
		t.Fatalf("synthesised message lost its subject:\n%s", raw)
	}
}

func TestSyncSurvivesExpiredDownloadURLs(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	addReceived(h, "expiring", "body text", true)
	h.api.ExpireDownloads(true) // signed URLs have gone stale

	res, err := h.sync.Sync(ctx)
	if err != nil {
		t.Fatalf("an expired raw URL must not fail the sync: %v", err)
	}
	if res.ReceivedStored != 1 {
		t.Fatalf("stored %d messages, want 1 (errors: %v)", res.ReceivedStored, res.Errors)
	}
	msgs := h.inbox(t)
	raw, err := h.acct.ReadBlob(msgs[0].BlobHash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "body text") {
		t.Fatalf("fell back to an empty message:\n%s", raw)
	}
}

func TestSyncStoresAttachmentsInFull(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := addReceived(h, "withfile", "see attached", false)
	h.api.Attach(m, resend.Attached{
		ID: "att-1", Filename: "report.pdf", ContentType: "application/pdf", Size: 9,
	}, []byte("PDF-BYTES"))

	if _, err := h.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	msgs := h.inbox(t)
	raw, err := h.acct.ReadBlob(msgs[0].BlobHash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "report.pdf") {
		t.Fatalf("attachment not embedded:\n%s", raw)
	}
	// The body is base64 encoded in the stored message.
	if !strings.Contains(string(raw), "UERGLUJZVEVT") {
		t.Fatalf("attachment content missing from the stored message:\n%s", raw)
	}
}

func TestSyncPlaceholdersUnavailableAttachments(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := addReceived(h, "lostfile", "see attached", false)
	h.api.Attach(m, resend.Attached{ID: "gone", Filename: "report.pdf", Size: 9}, nil)
	// Expired links on top of a missing body: the file is unrecoverable.
	h.api.ExpireDownloads(true)

	res, err := h.sync.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.ReceivedStored != 1 {
		t.Fatalf("stored %d messages, want 1 (errors: %v)", res.ReceivedStored, res.Errors)
	}
	raw, _ := h.acct.ReadBlob(h.inbox(t)[0].BlobHash)
	if !strings.Contains(string(raw), "report.pdf") {
		t.Fatalf("lost the record that something was attached:\n%s", raw)
	}
}

func TestSyncRetriesRateLimits(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	addReceived(h, "limited", "body", true)
	h.api.FailNext429(2) // the client must ride these out

	res, err := h.sync.Sync(ctx)
	if err != nil {
		t.Fatalf("sync should retry 429s: %v", err)
	}
	if res.ReceivedStored != 1 {
		t.Fatalf("stored %d messages after rate limiting", res.ReceivedStored)
	}
}

func TestSyncMarksSentMailRead(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := &fakeresend.Mail{}
	m.From = "hello@polygone.club"
	m.To = resend.Addrs{"customer@example.test"}
	m.Subject = "your receipt"
	m.MessageID = "<receipt@polygone.club>"
	m.Text = "thanks for your order"
	h.api.AddSent(m)

	res, err := h.sync.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.SentStored != 1 {
		t.Fatalf("stored %d sent messages, want 1 (errors: %v)", res.SentStored, res.Errors)
	}
	sent, _ := h.acct.Mailbox(ctx, store.Sent)
	msgs, _ := h.acct.Messages(ctx, sent.ID)
	if len(msgs) != 1 {
		t.Fatalf("%d messages in Sent", len(msgs))
	}
	seen := false
	for _, f := range msgs[0].Flags {
		if f == `\Seen` {
			seen = true
		}
	}
	if !seen {
		t.Error("the user's own sent mail should not show as unread")
	}
}

func TestSyncDedupesAgainstLocallyAppendedSentCopy(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// Ferry's SMTP server files its own copy at send time, with no Resend id
	// yet. Mail may add another via APPEND; both carry the same Message-ID.
	sent, _ := h.acct.Mailbox(ctx, store.Sent)
	raw := rawMail("receipt", "thanks")
	if _, err := h.acct.Append(ctx, sent.ID, &store.NewMessage{
		Raw:       raw,
		MessageID: "<receipt@example.test>",
		Subject:   "receipt",
	}); err != nil {
		t.Fatal(err)
	}

	m := &fakeresend.Mail{}
	m.From = "hello@polygone.club"
	m.To = resend.Addrs{"customer@example.test"}
	m.Subject = "receipt"
	m.MessageID = "<receipt@example.test>"
	m.Text = "thanks"
	api := h.api.AddSent(m)

	if _, err := h.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, _ := h.acct.Messages(ctx, sent.ID)
	if len(msgs) != 1 {
		t.Fatalf("%d copies in Sent, want 1 deduped by Message-ID", len(msgs))
	}
	if msgs[0].ResendID != api.ID {
		t.Fatalf("the local copy was not linked to the Resend id: %q != %q", msgs[0].ResendID, api.ID)
	}
}

func TestSyncSkipsMessagesThatFailAndKeepsGoing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	addReceived(h, "fine", "body", true)
	// A per-message failure must not abort the whole pass.
	h.api.FailDetail(500, "internal_error")

	res, err := h.sync.Sync(ctx)
	if err != nil {
		t.Fatalf("a single bad message should not fail the pass: %v", err)
	}
	if len(res.Errors) == 0 {
		t.Fatal("the failure was not reported")
	}

	// Once the API recovers, the message is stored on the next pass.
	h.api.FailDetail(0, "")
	if _, err := h.sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(h.inbox(t)); got != 1 {
		t.Fatalf("%d messages after recovery, want 1", got)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- h.sync.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run should return the context error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after the context was cancelled")
	}
}
