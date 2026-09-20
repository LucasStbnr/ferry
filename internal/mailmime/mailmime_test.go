package mailmime_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/LucasStbnr/ferry/internal/mailmime"
)

func TestBuildRoundTrip(t *testing.T) {
	sent := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)
	raw, err := mailmime.Build(&mailmime.Message{
		MessageID: "<abc@mysite.test>",
		From:      "Lucas <hello@mysite.test>",
		To:        []string{"friend@example.test"},
		Cc:        []string{"Cc Person <cc@example.test>"},
		Subject:   "Grüße aus München",
		Date:      sent,
		Text:      "plain body",
		HTML:      "<p>rich body</p>",
		Headers:   map[string]string{"In-Reply-To": "<prev@example.test>"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	idx := mailmime.Parse(raw)
	if idx.Subject != "Grüße aus München" {
		t.Errorf("subject = %q", idx.Subject)
	}
	if idx.MessageID != "<abc@mysite.test>" {
		t.Errorf("message-id = %q", idx.MessageID)
	}
	if !strings.Contains(idx.From, "hello@mysite.test") {
		t.Errorf("from = %q", idx.From)
	}
	if !strings.Contains(idx.To, "friend@example.test") || !strings.Contains(idx.To, "cc@example.test") {
		t.Errorf("to = %q (Cc should be indexed too)", idx.To)
	}
	if !idx.Date.Equal(sent) {
		t.Errorf("date = %v, want %v", idx.Date, sent)
	}
	if !strings.Contains(idx.Text, "plain body") {
		t.Errorf("indexed text = %q, want the text/plain alternative", idx.Text)
	}
	if !bytes.Contains(raw, []byte("In-Reply-To")) {
		t.Error("extra headers were dropped, threading would break")
	}
	if !bytes.Contains(raw, []byte("\r\n")) {
		t.Error("message must use CRLF line endings")
	}
}

func TestBuildNeverLeaksBcc(t *testing.T) {
	raw, err := mailmime.Build(&mailmime.Message{
		From:    "hello@mysite.test",
		To:      []string{"friend@example.test"},
		Bcc:     []string{"secret@example.test"},
		Subject: "hi",
		Text:    "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(raw), []byte("secret@example.test")) {
		t.Fatal("Bcc recipient appears in the stored message")
	}
}

func TestBuildWithAttachment(t *testing.T) {
	raw, err := mailmime.Build(&mailmime.Message{
		From:    "hello@mysite.test",
		To:      []string{"friend@example.test"},
		Subject: "invoice",
		Text:    "see attached",
		Attachments: []mailmime.Attachment{
			{Filename: "invoice.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4 fake")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("invoice.pdf")) {
		t.Fatal("attachment filename missing")
	}
	sub, err := mailmime.ParseSubmission(bytes.NewReader(raw), 10<<20)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(sub.Attachments) != 1 {
		t.Fatalf("%d attachments after round trip, want 1", len(sub.Attachments))
	}
	if got := string(sub.Attachments[0].Data); got != "%PDF-1.4 fake" {
		t.Fatalf("attachment body = %q", got)
	}
	if sub.Attachments[0].Filename != "invoice.pdf" {
		t.Fatalf("attachment filename = %q", sub.Attachments[0].Filename)
	}
}

func TestBuildRequiresFrom(t *testing.T) {
	if _, err := mailmime.Build(&mailmime.Message{To: []string{"a@b.test"}}); err == nil {
		t.Fatal("a message without From should not build")
	}
}

const appleMailSubmission = "From: Lucas <hello@mysite.test>\r\n" +
	"To: Friend <friend@example.test>\r\n" +
	"Cc: cc@example.test\r\n" +
	"Subject: Re: hello\r\n" +
	"Message-Id: <new@mysite.test>\r\n" +
	"In-Reply-To: <old@example.test>\r\n" +
	"References: <older@example.test> <old@example.test>\r\n" +
	"Date: Wed, 04 Mar 2026 09:30:00 +0000\r\n" +
	"Content-Type: multipart/alternative; boundary=\"B\"\r\n" +
	"Mime-Version: 1.0\r\n" +
	"\r\n" +
	"--B\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"plain reply\r\n" +
	"--B\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>html reply</p>\r\n" +
	"--B--\r\n"

func TestParseSubmission(t *testing.T) {
	sub, err := mailmime.ParseSubmission(strings.NewReader(appleMailSubmission), 10<<20)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sub.From != `"Lucas" <hello@mysite.test>` && sub.From != "Lucas <hello@mysite.test>" {
		t.Errorf("from = %q", sub.From)
	}
	if len(sub.To) != 1 || !strings.Contains(sub.To[0], "friend@example.test") {
		t.Errorf("to = %v", sub.To)
	}
	if len(sub.Cc) != 1 || sub.Cc[0] != "cc@example.test" {
		t.Errorf("cc = %v", sub.Cc)
	}
	if sub.Subject != "Re: hello" {
		t.Errorf("subject = %q", sub.Subject)
	}
	if sub.InReplyTo != "<old@example.test>" {
		t.Errorf("in-reply-to = %q", sub.InReplyTo)
	}
	if !strings.Contains(sub.Text, "plain reply") {
		t.Errorf("text = %q", sub.Text)
	}
	if !strings.Contains(sub.HTML, "html reply") {
		t.Errorf("html = %q", sub.HTML)
	}
	if !bytes.Equal(sub.Raw, []byte(appleMailSubmission)) {
		t.Error("Raw must be the exact submitted bytes")
	}
}

func TestParseSubmissionFlagsUnsupportedContent(t *testing.T) {
	signed := "From: a@b.test\r\nTo: c@d.test\r\nSubject: signed\r\n" +
		"Content-Type: multipart/signed; boundary=\"S\"; protocol=\"application/pkcs7-signature\"\r\n\r\n" +
		"--S\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--S\r\nContent-Type: application/pkcs7-signature\r\n\r\nc2lnbmF0dXJl\r\n" +
		"--S--\r\n"
	sub, err := mailmime.ParseSubmission(strings.NewReader(signed), 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Unsupported) == 0 {
		t.Fatal("an S/MIME signature must be reported as unsupported, not silently dropped")
	}
}

func TestParseSubmissionRejectsOversize(t *testing.T) {
	big := strings.Repeat("x", 4096)
	if _, err := mailmime.ParseSubmission(strings.NewReader(big), 1024); err == nil {
		t.Fatal("an oversized submission should be rejected")
	}
}

func TestParseIsForgiving(t *testing.T) {
	// Garbage must not panic and must still yield what was readable.
	idx := mailmime.Parse([]byte("Subject: broken\r\nthis is not a header\r\n\r\nbody"))
	if idx.Subject != "broken" {
		t.Errorf("subject = %q", idx.Subject)
	}
	if idx := mailmime.Parse(nil); idx.Subject != "" {
		t.Errorf("empty input produced %+v", idx)
	}
}

func TestParseStripsScriptAndStyle(t *testing.T) {
	raw := "Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<style>.a{color:red}</style><script>var secret=1</script><p>visible text</p>"
	idx := mailmime.Parse([]byte(raw))
	if strings.Contains(idx.Text, "secret") || strings.Contains(idx.Text, "color") {
		t.Fatalf("script/style content leaked into the index: %q", idx.Text)
	}
	if !strings.Contains(idx.Text, "visible text") {
		t.Fatalf("visible text missing: %q", idx.Text)
	}
}

func TestAddressHelpers(t *testing.T) {
	cases := []struct{ in, addr, domain string }{
		{"Lucas <hello@mysite.test>", "hello@mysite.test", "mysite.test"},
		{"plain@example.test", "plain@example.test", "example.test"},
		{"  spaced@Example.TEST  ", "spaced@Example.TEST", "example.test"},
		{"broken", "broken", ""},
	}
	for _, c := range cases {
		if got := mailmime.AddressOnly(c.in); got != c.addr {
			t.Errorf("AddressOnly(%q) = %q, want %q", c.in, got, c.addr)
		}
		if got := mailmime.Domain(c.in); got != c.domain {
			t.Errorf("Domain(%q) = %q, want %q", c.in, got, c.domain)
		}
	}
}
