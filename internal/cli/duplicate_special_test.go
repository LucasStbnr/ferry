package cli_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// stopDaemon shuts the daemon down and waits for it, so a repair that refuses
// to run alongside it can be exercised.
func (h *harness) stopDaemon() {
	h.t.Helper()
	if h.daemon == nil {
		return
	}
	_ = h.daemon.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = h.daemon.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = h.daemon.Process.Kill()
	}
	h.daemon = nil
}

// selectWithMail waits for the backfill to put mail in INBOX.
func (h *harness) selectWithMail(account string) *imapclient.Client {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		c := h.imapClient(account)
		sel, err := c.Select("INBOX", nil).Wait()
		if err != nil {
			h.t.Fatalf("select: %v", err)
		}
		if sel.NumMessages > 0 {
			return c
		}
		c.Close()
		if time.Now().After(deadline) {
			h.t.Fatalf("mail never arrived; daemon log:\n%s", h.logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestDoctorDuplicateTrash covers the mess a client makes
// when it declines the server's \Trash and creates its own folder: mail is
// deleted into "Deleted Messages" while Trash stays empty. doctor should see
// the duplicate and, on repair, put the messages where they belong.
func TestDoctorDuplicateTrash(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test builds the binary and starts a daemon")
	}
	h := newHarness(t)
	h.addReceived("stranded", "this went to the wrong trash")
	h.addAccount(testAccount)
	h.startDaemon()

	c := h.selectWithMail(testAccount)
	if err := c.Create("Deleted Messages", nil).Wait(); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.Move(imap.SeqSetNum(1), "Deleted Messages").Wait(); err != nil {
		t.Fatalf("move: %v", err)
	}
	c.Close()

	// While the daemon is up the repair must decline: imapd holds each
	// mailbox in memory and would not see the change.
	out := h.mustRun("doctor", "--repair")
	if !strings.Contains(out, "Deleted Messages") {
		t.Fatalf("doctor did not report the duplicate:\n%s", out)
	}
	if !strings.Contains(out, "not repaired while the daemon is running") {
		t.Fatalf("doctor repaired behind the running daemon:\n%s", out)
	}

	h.stopDaemon()

	out = h.mustRun("doctor", "--repair")
	if !strings.Contains(out, "merged 1 message(s)") {
		t.Fatalf("doctor did not merge the duplicate:\n%s", out)
	}

	// The message must be in Trash, and the duplicate gone.
	h.startDaemon()
	c = h.imapClient(testAccount)
	defer c.Close()

	sel, err := c.Select("Trash", nil).Wait()
	if err != nil {
		t.Fatalf("select Trash: %v", err)
	}
	if sel.NumMessages != 1 {
		t.Fatalf("Trash holds %d messages, want 1", sel.NumMessages)
	}
	msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{Envelope: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].Envelope.Subject != "stranded" {
		t.Errorf("subject = %q, want %q", msgs[0].Envelope.Subject, "stranded")
	}

	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range boxes {
		if b.Mailbox == "Deleted Messages" {
			t.Errorf("the duplicate mailbox is still listed")
		}
	}
}
