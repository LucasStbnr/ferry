package imapd_test

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/LucasStbnr/ferry/internal/account"
	"github.com/LucasStbnr/ferry/internal/imapd"
	"github.com/LucasStbnr/ferry/internal/secrets"
	"github.com/LucasStbnr/ferry/internal/store"
	"github.com/LucasStbnr/ferry/internal/tlsutil"
)

const appPassword = "test-pass-word-0001"

type fixture struct {
	db     *store.DB
	server *imapd.Server
	addr   string
	tls    *tls.Config
}

// testLogger sends server logs to the test output, so a protocol-level
// failure shows up next to the assertion that caught it.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("server: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func newFixture(t *testing.T, accounts ...string) *fixture {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "ferry.db"), filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mgr := account.NewManager(db, &secrets.Memory{}, nil)
	hash, err := account.HashPassword(appPassword)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range accounts {
		a, err := db.CreateAccount(ctx, name, name+"@example.test", hash)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Account(a).EnsureDefaultMailboxes(ctx); err != nil {
			t.Fatal(err)
		}
	}

	bundle, err := tlsutil.EnsureBundle(filepath.Join(dir, "tls"), []string{"localhost"})
	if err != nil {
		t.Fatalf("tls: %v", err)
	}
	serverTLS, err := bundle.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}

	srv, err := imapd.New(imapd.Options{
		Addr:      "127.0.0.1:0",
		TLSConfig: serverTLS,
		DB:        db,
		Auth:      mgr,
		Logger:    testLogger(t),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
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
	return &fixture{db: db, server: srv, addr: ln.Addr().String(), tls: clientTLS}
}

func (f *fixture) dial(t *testing.T, account string) *imapclient.Client {
	t.Helper()
	return f.dialWith(t, account, nil)
}

func (f *fixture) dialWith(t *testing.T, account string, h *imapclient.UnilateralDataHandler) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialTLS(f.addr, &imapclient.Options{TLSConfig: f.tls, UnilateralDataHandler: h})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	if account != "" {
		if err := c.Login(account, appPassword).Wait(); err != nil {
			t.Fatalf("login as %s: %v", account, err)
		}
	}
	return c
}

func (f *fixture) acct(t *testing.T, name string) *store.AccountStore {
	t.Helper()
	a, err := f.db.AccountByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return f.db.Account(a)
}

func (f *fixture) put(t *testing.T, name, mailbox, subject, body string, flags ...string) {
	t.Helper()
	ctx := context.Background()
	as := f.acct(t, name)
	mb, err := as.Mailbox(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: sender@example.test\r\n" +
		"To: " + name + "@example.test\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-Id: <" + subject + "@example.test>\r\n" +
		"Date: Wed, 04 Mar 2026 09:30:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		body + "\r\n"
	if _, err := as.Append(ctx, mb.ID, &store.NewMessage{
		Raw:        []byte(raw),
		MessageID:  "<" + subject + "@example.test>",
		Subject:    subject,
		From:       "sender@example.test",
		SearchText: body,
		Flags:      flags,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilitiesAppleMailNeeds(t *testing.T) {
	f := newFixture(t, "acct")
	c := f.dial(t, "acct")

	caps := c.Caps()
	// Without these, clients fall back to slow or wrong behaviour: guessing
	// folder roles by name, re-downloading on every move, polling instead of
	// idling.
	for _, want := range []imap.Cap{
		imap.CapIMAP4rev1, imap.CapNamespace, imap.CapUIDPlus,
		imap.CapMove, imap.CapIdle, imap.CapListExtended, imap.CapSpecialUse,
	} {
		if !caps.Has(want) {
			t.Errorf("missing capability %s", want)
		}
	}
}

func TestLoginIsRejectedForBadCredentials(t *testing.T) {
	f := newFixture(t, "acct")

	c := f.dial(t, "")
	if err := c.Login("acct", "wrong-password").Wait(); err == nil {
		t.Fatal("a wrong password was accepted")
	}

	c2 := f.dial(t, "")
	if err := c2.Login("nosuchaccount", appPassword).Wait(); err == nil {
		t.Fatal("an unknown account was accepted")
	}
}

func TestListShowsSpecialUseFolders(t *testing.T) {
	f := newFixture(t, "acct")
	c := f.dial(t, "acct")

	boxes, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string][]imap.MailboxAttr{}
	for _, b := range boxes {
		got[b.Mailbox] = b.Attrs
		if b.Delim != '/' {
			t.Errorf("mailbox %s has delimiter %q, want /", b.Mailbox, b.Delim)
		}
	}
	for _, want := range []string{"INBOX", "Sent", "Drafts", "Archive", "Trash", "Junk"} {
		if _, ok := got[want]; !ok {
			t.Errorf("mailbox %s missing from LIST", want)
		}
	}
	if boxes[0].Mailbox != "INBOX" {
		t.Errorf("first mailbox is %s, want INBOX", boxes[0].Mailbox)
	}
	// Mail binds its Sent/Trash buttons using these attributes.
	for name, attr := range map[string]imap.MailboxAttr{
		"Sent": imap.MailboxAttrSent, "Drafts": imap.MailboxAttrDrafts,
		"Trash": imap.MailboxAttrTrash, "Junk": imap.MailboxAttrJunk,
		"Archive": imap.MailboxAttrArchive,
	} {
		found := false
		for _, a := range got[name] {
			if a == attr {
				found = true
			}
		}
		if !found {
			t.Errorf("mailbox %s is missing the %s attribute (got %v)", name, attr, got[name])
		}
	}
}

func TestNamespace(t *testing.T) {
	f := newFixture(t, "acct")
	c := f.dial(t, "acct")

	ns, err := c.Namespace().Wait()
	if err != nil {
		t.Fatalf("namespace: %v", err)
	}
	if len(ns.Personal) != 1 || ns.Personal[0].Delim != '/' {
		t.Fatalf("namespace = %+v", ns)
	}
}

func TestSelectFetchAndFlags(t *testing.T) {
	f := newFixture(t, "acct")
	f.put(t, "acct", "INBOX", "hello", "the quick brown fox")
	f.put(t, "acct", "INBOX", "second", "another message")

	c := f.dial(t, "acct")
	sel, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if sel.NumMessages != 2 {
		t.Fatalf("EXISTS = %d, want 2", sel.NumMessages)
	}
	if sel.UIDValidity == 0 {
		t.Error("UIDVALIDITY must be non-zero")
	}

	msgs, err := c.Fetch(imap.SeqSetNum(1, 2), &imap.FetchOptions{
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		RFC822Size:   true,
		UID:          true,
		BodySection:  []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("fetched %d messages, want 2", len(msgs))
	}
	if msgs[0].Envelope.Subject != "hello" {
		t.Errorf("subject = %q", msgs[0].Envelope.Subject)
	}
	if msgs[0].UID != 1 {
		t.Errorf("uid = %d, want 1", msgs[0].UID)
	}
	if msgs[0].RFC822Size == 0 {
		t.Error("RFC822.SIZE is zero")
	}
	body := msgs[0].FindBodySection(&imap.FetchItemBodySection{Peek: true})
	if !strings.Contains(string(body), "quick brown fox") {
		t.Errorf("body = %q", body)
	}
	// A peeking fetch must not mark the message read.
	for _, fl := range msgs[0].Flags {
		if fl == imap.FlagSeen {
			t.Error("BODY.PEEK marked the message as seen")
		}
	}
}

func TestFetchWithoutPeekMarksSeenAndPersists(t *testing.T) {
	f := newFixture(t, "acct")
	f.put(t, "acct", "INBOX", "unread", "body")

	c := f.dial(t, "acct")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}}, // no peek
	}).Collect(); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	// The flag must have reached the database, not just this connection.
	ctx := context.Background()
	as := f.acct(t, "acct")
	mb, _ := as.Mailbox(ctx, "INBOX")
	msgs, _ := as.Messages(ctx, mb.ID)
	found := false
	for _, fl := range msgs[0].Flags {
		if fl == `\Seen` {
			found = true
		}
	}
	if !found {
		t.Fatal("reading a message did not persist \\Seen")
	}
}

func TestStoreFlagsRoundTrip(t *testing.T) {
	f := newFixture(t, "acct")
	f.put(t, "acct", "INBOX", "flagme", "body")

	c := f.dial(t, "acct")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(imap.SeqSetNum(1), &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagFlagged, imap.FlagAnswered},
	}, nil).Close(); err != nil {
		t.Fatalf("store: %v", err)
	}

	// Reconnecting proves the flags were persisted, not just cached.
	c2 := f.dial(t, "acct")
	if _, err := c2.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	msgs, err := c2.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{Flags: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := map[imap.Flag]bool{}
	for _, fl := range msgs[0].Flags {
		got[fl] = true
	}
	if !got[imap.FlagFlagged] || !got[imap.FlagAnswered] {
		t.Fatalf("flags after reconnect = %v", msgs[0].Flags)
	}
}

func TestSearch(t *testing.T) {
	f := newFixture(t, "acct")
	f.put(t, "acct", "INBOX", "invoice", "your quarterly invoice is attached")
	f.put(t, "acct", "INBOX", "lunch", "are you free thursday")
	f.put(t, "acct", "INBOX", "seenone", "already read", `\Seen`)

	c := f.dial(t, "acct")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	data, err := c.Search(&imap.SearchCriteria{Text: []string{"quarterly"}}, nil).Wait()
	if err != nil {
		t.Fatalf("search text: %v", err)
	}
	if got := data.AllSeqNums(); len(got) != 1 || got[0] != 1 {
		t.Errorf("TEXT search matched %v, want [1]", got)
	}

	data, err = c.Search(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "lunch"}},
	}, nil).Wait()
	if err != nil {
		t.Fatalf("search subject: %v", err)
	}
	if got := data.AllSeqNums(); len(got) != 1 || got[0] != 2 {
		t.Errorf("SUBJECT search matched %v, want [2]", got)
	}

	data, err = c.Search(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil {
		t.Fatalf("search unseen: %v", err)
	}
	if got := data.AllSeqNums(); len(got) != 2 {
		t.Errorf("UNSEEN matched %v, want 2 messages", got)
	}

	// ESEARCH: the counted form Mail uses for its unread badge.
	data, err = c.Search(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}},
		&imap.SearchOptions{ReturnCount: true}).Wait()
	if err != nil {
		t.Fatalf("esearch: %v", err)
	}
	if data.Count != 2 {
		t.Errorf("ESEARCH COUNT = %d, want 2", data.Count)
	}
}

func TestAppendMoveAndExpunge(t *testing.T) {
	f := newFixture(t, "acct")
	c := f.dial(t, "acct")

	raw := "From: me@example.test\r\nTo: you@example.test\r\nSubject: draft\r\n" +
		"Message-Id: <draft@example.test>\r\n\r\nhalf written\r\n"
	aw := c.Append("Drafts", int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagDraft}})
	if _, err := aw.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	appended, err := aw.Wait()
	if err != nil {
		t.Fatalf("append wait: %v", err)
	}
	// UIDPLUS: Mail uses the returned UID to track the draft it just saved.
	if appended.UID != 1 || appended.UIDValidity == 0 {
		t.Fatalf("append data = %+v", appended)
	}

	if _, err := c.Select("Drafts", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Move(imap.SeqSetNum(1), "Trash").Wait(); err != nil {
		t.Fatalf("move: %v", err)
	}

	sel, err := c.Select("Trash", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.NumMessages != 1 {
		t.Fatalf("Trash has %d messages after MOVE, want 1", sel.NumMessages)
	}

	if err := c.Store(imap.SeqSetNum(1), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge().Close(); err != nil {
		t.Fatalf("expunge: %v", err)
	}

	sel, err = c.Select("Trash", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.NumMessages != 0 {
		t.Fatalf("Trash has %d messages after expunge, want 0", sel.NumMessages)
	}
}

func TestCopyReportsUIDPlusData(t *testing.T) {
	f := newFixture(t, "acct")
	f.put(t, "acct", "INBOX", "copyme", "body")

	c := f.dial(t, "acct")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.Copy(imap.SeqSetNum(1), "Archive").Wait()
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if data == nil || data.UIDValidity == 0 {
		t.Fatalf("copy data = %+v, want UIDPLUS information", data)
	}
	sel, err := c.Select("Archive", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.NumMessages != 1 {
		t.Fatalf("Archive has %d messages after COPY", sel.NumMessages)
	}
}

func TestCreateRenameDeleteMailbox(t *testing.T) {
	f := newFixture(t, "acct")
	c := f.dial(t, "acct")

	if err := c.Create("Clients/Acme", nil).Wait(); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.Rename("Clients/Acme", "Clients/Acme Corp", nil).Wait(); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := c.Subscribe("Clients/Acme Corp").Wait(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	boxes, err := c.List("", "Clients/*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 1 || boxes[0].Mailbox != "Clients/Acme Corp" {
		t.Fatalf("list after rename = %+v", boxes)
	}
	if err := c.Delete("Clients/Acme Corp").Wait(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.Delete("INBOX").Wait(); err == nil {
		t.Fatal("deleting INBOX should be refused")
	}
}

func TestStatus(t *testing.T) {
	f := newFixture(t, "acct")
	f.put(t, "acct", "INBOX", "one", "body")
	f.put(t, "acct", "INBOX", "two", "body", `\Seen`)

	c := f.dial(t, "acct")
	data, err := c.Status("INBOX", &imap.StatusOptions{
		NumMessages: true, NumUnseen: true, UIDNext: true, UIDValidity: true,
	}).Wait()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if data.NumMessages == nil || *data.NumMessages != 2 {
		t.Errorf("MESSAGES = %v, want 2", data.NumMessages)
	}
	if data.NumUnseen == nil || *data.NumUnseen != 1 {
		t.Errorf("UNSEEN = %v, want 1", data.NumUnseen)
	}
	if data.UIDNext != 3 {
		t.Errorf("UIDNEXT = %d, want 3", data.UIDNext)
	}
}

func TestAccountsCannotSeeEachOther(t *testing.T) {
	f := newFixture(t, "alpha", "beta")
	f.put(t, "alpha", "INBOX", "alpha-secret", "confidential")

	c := f.dial(t, "beta")
	sel, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.NumMessages != 0 {
		t.Fatalf("beta sees %d of alpha's messages", sel.NumMessages)
	}
	data, err := c.Search(&imap.SearchCriteria{Text: []string{"confidential"}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := data.AllSeqNums(); len(got) != 0 {
		t.Fatalf("beta's search found %v of alpha's messages", got)
	}
}

func TestIdleReceivesNewMail(t *testing.T) {
	f := newFixture(t, "acct")
	updates := make(chan uint32, 8)
	c := f.dialWith(t, "acct", &imapclient.UnilateralDataHandler{
		Mailbox: func(u *imapclient.UnilateralDataMailbox) {
			if u.NumMessages != nil {
				updates <- *u.NumMessages
			}
		},
	})
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	idle, err := c.Idle()
	if err != nil {
		t.Fatalf("idle: %v", err)
	}

	// New mail arriving through the sync engine must reach an idling client.
	f.put(t, "acct", "INBOX", "breaking", "news")
	ctx := context.Background()
	as := f.acct(t, "acct")
	mb, _ := as.Mailbox(ctx, "INBOX")
	f.server.MailboxChanged("acct", mb.ID)

	select {
	case n := <-updates:
		if n != 1 {
			t.Fatalf("EXISTS = %d, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no unsolicited EXISTS reached the idling client")
	}
	if err := idle.Close(); err != nil {
		t.Fatalf("close idle: %v", err)
	}
}

func TestUIDFetchAndUIDSearch(t *testing.T) {
	f := newFixture(t, "acct")
	f.put(t, "acct", "INBOX", "first", "alpha body")
	f.put(t, "acct", "INBOX", "second", "beta body")

	c := f.dial(t, "acct")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	msgs, err := c.Fetch(imap.UIDSetNum(2), &imap.FetchOptions{Envelope: true, UID: true}).Collect()
	if err != nil {
		t.Fatalf("uid fetch: %v", err)
	}
	if len(msgs) != 1 || msgs[0].UID != 2 || msgs[0].Envelope.Subject != "second" {
		t.Fatalf("uid fetch returned %+v", msgs)
	}

	data, err := c.UIDSearch(&imap.SearchCriteria{Text: []string{"beta"}}, nil).Wait()
	if err != nil {
		t.Fatalf("uid search: %v", err)
	}
	uids, ok := data.All.(imap.UIDSet)
	if !ok {
		t.Fatalf("UID SEARCH returned %T, want a UID set", data.All)
	}
	if !uids.Contains(2) {
		t.Fatalf("UID SEARCH result = %v, want uid 2", uids)
	}
}

// TestMailboxNamesWithLeadingDelimiter reproduces the bug that made deleting
// mail silently do nothing in Apple Mail.
//
// Mail builds mailbox paths as prefix + delimiter + name. With an empty path
// prefix that yields "/Trash", the server answered NONEXISTENT, and because
// Mail's delete is a move to Trash it failed with no visible error at all;
// the message simply stayed where it was.
func TestMailboxNamesWithLeadingDelimiter(t *testing.T) {
	f := newFixture(t, "acct")
	f.put(t, "acct", "INBOX", "deleteme", "body")
	c := f.dial(t, "acct")

	// STATUS is what Mail uses to discover the special folders.
	for _, name := range []string{"/Trash", "/Sent", "/Drafts", "/Archive", "/Junk"} {
		if _, err := c.Status(name, &imap.StatusOptions{NumMessages: true}).Wait(); err != nil {
			t.Errorf("STATUS %s: %v", name, err)
		}
	}

	// And the delete itself: select INBOX, move to "/Trash".
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Move(imap.SeqSetNum(1), "/Trash").Wait(); err != nil {
		t.Fatalf("MOVE to /Trash: %v", err)
	}
	sel, err := c.Select("/Trash", nil).Wait()
	if err != nil {
		t.Fatalf("SELECT /Trash: %v", err)
	}
	if sel.NumMessages != 1 {
		t.Fatalf("Trash has %d messages after the move, want 1", sel.NumMessages)
	}

	// The message must really be gone from INBOX, not copied.
	if sel, err = c.Select("/INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if sel.NumMessages != 0 {
		t.Fatalf("INBOX still has %d messages after the move", sel.NumMessages)
	}
}

func TestLeadingDelimiterOnEveryMailboxCommand(t *testing.T) {
	f := newFixture(t, "acct")
	c := f.dial(t, "acct")

	if err := c.Create("/Clients", nil).Wait(); err != nil {
		t.Fatalf("CREATE /Clients: %v", err)
	}
	// It must be created as "Clients", not as an empty-named child.
	boxes, err := c.List("", "Clients", nil).Collect()
	if err != nil || len(boxes) != 1 {
		t.Fatalf("list after CREATE /Clients = %+v, %v", boxes, err)
	}
	if err := c.Subscribe("/Clients").Wait(); err != nil {
		t.Errorf("SUBSCRIBE /Clients: %v", err)
	}
	if err := c.Rename("/Clients", "/Customers", nil).Wait(); err != nil {
		t.Errorf("RENAME /Clients: %v", err)
	}
	if err := c.Delete("/Customers").Wait(); err != nil {
		t.Errorf("DELETE /Customers: %v", err)
	}
}
