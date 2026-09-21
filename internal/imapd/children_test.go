package imapd_test

import (
	"slices"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// attrsOf indexes a LIST result by mailbox name.
func attrsOf(t *testing.T, boxes []*imap.ListData) map[string][]imap.MailboxAttr {
	t.Helper()
	out := make(map[string][]imap.MailboxAttr, len(boxes))
	for _, b := range boxes {
		out[b.Mailbox] = b.Attrs
	}
	return out
}

// TestListReportsChildAttributes pins the CHILDREN attributes on LIST.
//
// They are not cosmetic: a client that cannot tell a leaf from an unexplored
// parent may distrust the whole listing, and Apple Mail answers that by
// ignoring the \Trash it was offered and creating its own "Deleted Messages".
func TestListReportsChildAttributes(t *testing.T) {
	f := newFixture(t, "acct")
	c := f.dial(t, "acct")
	defer c.Close()

	if !c.Caps().Has(imap.CapChildren) {
		t.Error("server does not advertise CHILDREN")
	}

	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	attrs := attrsOf(t, boxes)
	for _, name := range []string{"INBOX", "Archive", "Drafts", "Junk", "Sent", "Trash"} {
		got, ok := attrs[name]
		if !ok {
			t.Errorf("%s missing from LIST", name)
			continue
		}
		if !slices.Contains(got, imap.MailboxAttrHasNoChildren) {
			t.Errorf("%s attrs = %v, want \\HasNoChildren", name, got)
		}
	}

	// A nested mailbox must flip its parent over to \HasChildren.
	if err := c.Create("Archive/2026", nil).Wait(); err != nil {
		t.Fatalf("create: %v", err)
	}
	boxes, err = c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	attrs = attrsOf(t, boxes)
	if got := attrs["Archive"]; !slices.Contains(got, imap.MailboxAttrHasChildren) {
		t.Errorf("Archive attrs = %v, want \\HasChildren", got)
	}
	if got := attrs["Archive"]; slices.Contains(got, imap.MailboxAttrHasNoChildren) {
		t.Errorf("Archive attrs = %v, must not claim \\HasNoChildren", got)
	}
	if got := attrs["Archive/2026"]; !slices.Contains(got, imap.MailboxAttrHasNoChildren) {
		t.Errorf("Archive/2026 attrs = %v, want \\HasNoChildren", got)
	}
}

// TestListSpecialUseSelectorIgnoresChildAttributes guards the filter that
// selects special-use mailboxes. It used to key off the attribute list being
// empty, which every mailbox now defeats by carrying a child attribute.
func TestListSpecialUseSelectorIgnoresChildAttributes(t *testing.T) {
	f := newFixture(t, "acct")
	c := f.dial(t, "acct")
	defer c.Close()

	boxes, err := c.List("", "*", &imap.ListOptions{SelectSpecialUse: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(boxes))
	for _, b := range boxes {
		got = append(got, b.Mailbox)
	}
	slices.Sort(got)

	want := []string{"Archive", "Drafts", "Junk", "Sent", "Trash"}
	if !slices.Equal(got, want) {
		t.Errorf("special-use mailboxes = %v, want %v (INBOX has no special use)", got, want)
	}
}
