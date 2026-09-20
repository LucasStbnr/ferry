package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/LucasStbnr/ferry/internal/store"
)

func openTest(t *testing.T) *store.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "ferry.db"), filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newAccount(t *testing.T, db *store.DB, name string) *store.AccountStore {
	t.Helper()
	ctx := context.Background()
	a, err := db.CreateAccount(ctx, name, name+"@example.test", "hash")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	as := db.Account(a)
	if err := as.EnsureDefaultMailboxes(ctx); err != nil {
		t.Fatalf("default mailboxes: %v", err)
	}
	return as
}

func appendTo(t *testing.T, as *store.AccountStore, mailbox, body string, nm *store.NewMessage) uint32 {
	t.Helper()
	ctx := context.Background()
	mb, err := as.Mailbox(ctx, mailbox)
	if err != nil {
		t.Fatalf("mailbox %s: %v", mailbox, err)
	}
	if nm == nil {
		nm = &store.NewMessage{}
	}
	nm.Raw = []byte(body)
	uid, err := as.Append(ctx, mb.ID, nm)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return uid
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		db, err := store.Open(ctx, filepath.Join(dir, "ferry.db"), filepath.Join(dir, "blobs"))
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := db.Check(ctx); err != nil {
			t.Fatalf("integrity: %v", err)
		}
		db.Close()
	}
}

func TestAccountsAreIsolated(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	a := newAccount(t, db, "alpha")
	b := newAccount(t, db, "beta")

	appendTo(t, a, store.Inbox, "Subject: for alpha\r\n\r\nbody\r\n", &store.NewMessage{ResendID: "rcv-1"})

	// The same mailbox name in the other account is a different mailbox.
	bInbox, err := b.Mailbox(ctx, store.Inbox)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := b.Messages(ctx, bInbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("beta sees %d of alpha's messages", len(msgs))
	}

	// Nor can beta reach alpha's message by id.
	aInbox, _ := a.Mailbox(ctx, store.Inbox)
	aMsgs, _ := a.Messages(ctx, aInbox.ID)
	if _, err := b.Message(ctx, aMsgs[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("beta read alpha's message: err = %v", err)
	}
	if found, _ := b.FindByResendID(ctx, "rcv-1"); found != nil {
		t.Fatal("beta found alpha's message by resend id")
	}
}

func TestUIDsAreMonotonicPerMailbox(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	as := newAccount(t, db, "acct")

	u1 := appendTo(t, as, store.Inbox, "Subject: one\r\n\r\na\r\n", nil)
	u2 := appendTo(t, as, store.Inbox, "Subject: two\r\n\r\nb\r\n", nil)
	if u1 != 1 || u2 != 2 {
		t.Fatalf("uids = %d, %d; want 1, 2", u1, u2)
	}
	// A fresh mailbox starts at 1 again.
	if u := appendTo(t, as, store.Sent, "Subject: three\r\n\r\nc\r\n", nil); u != 1 {
		t.Fatalf("Sent first uid = %d, want 1", u)
	}

	// UIDs are never reused after an expunge.
	inbox, _ := as.Mailbox(ctx, store.Inbox)
	msgs, _ := as.Messages(ctx, inbox.ID)
	if err := as.Expunge(ctx, []int64{msgs[1].ID}); err != nil {
		t.Fatal(err)
	}
	if u := appendTo(t, as, store.Inbox, "Subject: four\r\n\r\nd\r\n", nil); u != 3 {
		t.Fatalf("uid after expunge = %d, want 3", u)
	}
}

func TestExpungeWritesTombstoneAndKeepsBlobsRefcounted(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	as := newAccount(t, db, "acct")

	raw := "Subject: hello\r\n\r\nbody\r\n"
	appendTo(t, as, store.Inbox, raw, &store.NewMessage{ResendID: "rcv-9", MessageID: "<m9@x>"})
	hash := store.HashBytes([]byte(raw))
	if !as.HasBlob(hash) {
		t.Fatal("blob missing after append")
	}

	inbox, _ := as.Mailbox(ctx, store.Inbox)
	msgs, _ := as.Messages(ctx, inbox.ID)

	// Copy first: the blob must survive deleting one of the two references.
	archive, _ := as.Mailbox(ctx, store.Archive)
	if _, err := as.Copy(ctx, []int64{msgs[0].ID}, archive.ID); err != nil {
		t.Fatal(err)
	}
	if err := as.Expunge(ctx, []int64{msgs[0].ID}); err != nil {
		t.Fatal(err)
	}
	if !as.HasBlob(hash) {
		t.Fatal("blob removed while a copy still references it")
	}

	tombstoned, err := as.Tombstoned(ctx, "rcv-9")
	if err != nil {
		t.Fatal(err)
	}
	if !tombstoned {
		t.Fatal("no tombstone written for an expunged Resend message")
	}

	// Removing the last reference drops the file.
	aMsgs, _ := as.Messages(ctx, archive.ID)
	if err := as.Expunge(ctx, []int64{aMsgs[0].ID}); err != nil {
		t.Fatal(err)
	}
	if as.HasBlob(hash) {
		t.Fatal("blob kept after the last reference went away")
	}
}

func TestFlags(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	as := newAccount(t, db, "acct")
	appendTo(t, as, store.Inbox, "Subject: flags\r\n\r\nx\r\n", nil)

	inbox, _ := as.Mailbox(ctx, store.Inbox)
	msgs, _ := as.Messages(ctx, inbox.ID)
	id := msgs[0].ID

	// System flags are case-insensitive and canonicalised on the way in.
	got, err := as.StoreFlags(ctx, []int64{id}, store.FlagsAdd, []string{`\seen`, "Important"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Important", `\Seen`}
	if len(got[id]) != 2 || got[id][0] != want[0] || got[id][1] != want[1] {
		t.Fatalf("flags = %v, want %v", got[id], want)
	}

	if got, _ = as.StoreFlags(ctx, []int64{id}, store.FlagsDel, []string{`\SEEN`}); len(got[id]) != 1 {
		t.Fatalf("after delete flags = %v", got[id])
	}
	if got, _ = as.StoreFlags(ctx, []int64{id}, store.FlagsSet, []string{`\Flagged`}); len(got[id]) != 1 || got[id][0] != `\Flagged` {
		t.Fatalf("after set flags = %v", got[id])
	}

	st, err := as.Stats(ctx, inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Messages != 1 || st.Unseen != 1 {
		t.Fatalf("stats = %+v, want 1 message 1 unseen", st)
	}
}

func TestMoveKeepsOneCopy(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	as := newAccount(t, db, "acct")
	appendTo(t, as, store.Inbox, "Subject: move me\r\n\r\nx\r\n", nil)

	inbox, _ := as.Mailbox(ctx, store.Inbox)
	trash, _ := as.Mailbox(ctx, store.Trash)
	msgs, _ := as.Messages(ctx, inbox.ID)

	res, err := as.Move(ctx, []int64{msgs[0].ID}, trash.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].SourceUID != 1 || res[0].DestUID != 1 {
		t.Fatalf("move result = %+v", res)
	}
	if left, _ := as.Messages(ctx, inbox.ID); len(left) != 0 {
		t.Fatalf("%d messages left in INBOX after move", len(left))
	}
	if moved, _ := as.Messages(ctx, trash.ID); len(moved) != 1 {
		t.Fatalf("%d messages in Trash after move", len(moved))
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	as := newAccount(t, db, "acct")

	appendTo(t, as, store.Inbox, "Subject: invoice\r\n\r\nx\r\n", &store.NewMessage{
		Subject: "Quarterly invoice", From: "billing@vendor.test", SearchText: "please find the invoice attached",
	})
	appendTo(t, as, store.Inbox, "Subject: lunch\r\n\r\ny\r\n", &store.NewMessage{
		Subject: "Lunch?", From: "friend@example.test", SearchText: "are you free on thursday",
	})

	inbox, _ := as.Mailbox(ctx, store.Inbox)
	hits, err := as.Search(ctx, inbox.ID, "invoice")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search invoice: %d hits, want 1", len(hits))
	}

	// FTS5 operators typed by a user are literal, not syntax.
	if _, err := as.Search(ctx, inbox.ID, `NEAR( "unbalanced`); err != nil {
		t.Fatalf("search with FTS operators must not error: %v", err)
	}

	// An expunged message stops matching.
	if err := as.Expunge(ctx, hits); err != nil {
		t.Fatal(err)
	}
	if hits, _ = as.Search(ctx, inbox.ID, "invoice"); len(hits) != 0 {
		t.Fatalf("expunged message still matches: %v", hits)
	}
}

func TestMailboxLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	as := newAccount(t, db, "acct")

	if err := as.CreateMailbox(ctx, "Clients/Acme", ""); err != nil {
		t.Fatal(err)
	}
	// The parent is created implicitly.
	if _, err := as.Mailbox(ctx, "Clients"); err != nil {
		t.Fatalf("parent mailbox not created: %v", err)
	}

	if err := as.RenameMailbox(ctx, "Clients", "Customers"); err != nil {
		t.Fatal(err)
	}
	if _, err := as.Mailbox(ctx, "Customers/Acme"); err != nil {
		t.Fatalf("descendant not renamed: %v", err)
	}

	if err := as.DeleteMailbox(ctx, "Customers"); err == nil {
		t.Fatal("deleting a mailbox with children should fail")
	}
	if err := as.DeleteMailbox(ctx, store.Inbox); err == nil {
		t.Fatal("deleting INBOX should fail")
	}

	// INBOX is case-insensitive.
	if _, err := as.Mailbox(ctx, "inbox"); err != nil {
		t.Fatalf("INBOX must be case-insensitive: %v", err)
	}
	if err := as.CreateMailbox(ctx, "Bad\x00Name", ""); err == nil {
		t.Fatal("control characters should be rejected in mailbox names")
	}
}

func TestSyncStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	as := newAccount(t, db, "acct")

	st, err := as.SyncState(ctx, store.KindReceived)
	if err != nil {
		t.Fatal(err)
	}
	if st.NewestID != "" || st.BackfillDone {
		t.Fatalf("fresh state = %+v", st)
	}

	st.NewestID = "rcv-10"
	st.BackfillCursor = "rcv-1"
	st.BackfillDone = true
	st.LastSyncAt = time.Unix(1700000000, 0)
	if err := as.SaveSyncState(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := as.SyncState(ctx, store.KindReceived)
	if err != nil {
		t.Fatal(err)
	}
	if got.NewestID != "rcv-10" || got.BackfillCursor != "rcv-1" || !got.BackfillDone || !got.LastSyncAt.Equal(st.LastSyncAt) {
		t.Fatalf("round trip = %+v, want %+v", got, st)
	}
}

func TestGCBlobsRemovesUnreferencedFiles(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	as := newAccount(t, db, "acct")
	appendTo(t, as, store.Inbox, "Subject: keep\r\n\r\nx\r\n", nil)

	// A blob written without a row is what a crash mid-append leaves behind.
	inbox, _ := as.Mailbox(ctx, store.Inbox)
	orphan := &store.NewMessage{Raw: []byte("Subject: orphan\r\n\r\ny\r\n")}
	if _, err := as.Append(ctx, inbox.ID, orphan); err != nil {
		t.Fatal(err)
	}
	msgs, _ := as.Messages(ctx, inbox.ID)
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, msgs[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM blobs WHERE hash = ?`, msgs[1].BlobHash); err != nil {
		t.Fatal(err)
	}

	removed, freed, err := as.GCBlobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || freed == 0 {
		t.Fatalf("gc removed %d blobs freeing %d bytes, want 1 and non-zero", removed, freed)
	}
	if !as.HasBlob(msgs[0].BlobHash) {
		t.Fatal("gc removed a referenced blob")
	}
}
