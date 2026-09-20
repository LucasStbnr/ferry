// Package mailmime converts between Resend's structured JSON and RFC 5322
// messages.
//
// Two directions matter. Inbound, Resend sometimes offers the original raw
// message and sometimes only html/text plus headers; when the raw is missing
// or its signed URL has expired, Build synthesises a faithful message so that
// a client always has something complete to render. Outbound, the client
// submits real MIME over SMTP and ParseSubmission reduces it to the fields
// Resend's send API accepts.
package mailmime

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"strings"
	"time"

	gomail "github.com/emersion/go-message/mail"
	"github.com/google/uuid"
)

// Attachment is a file to embed in a synthesised message.
type Attachment struct {
	Filename    string
	ContentType string
	// ContentID, when set, marks the part as inline and lets HTML reference it
	// with cid:.
	ContentID string
	Data      []byte
}

// Message is the input to Build: everything Ferry knows about a message that
// it must render as RFC 5322.
type Message struct {
	MessageID string
	From      string
	To        []string
	Cc        []string
	Bcc       []string
	ReplyTo   []string
	Subject   string
	Date      time.Time
	Text      string
	HTML      string
	// Headers are extra headers from Resend, such as In-Reply-To and
	// References, which keep threading intact in Mail.
	Headers     map[string]string
	Attachments []Attachment
}

// headersNotCopied are headers Build always writes itself. Copying them from
// the Headers map would produce a duplicate, which some clients treat as
// malformed.
var headersNotCopied = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true, "reply-to": true,
	"subject": true, "date": true, "message-id": true, "mime-version": true,
	"content-type": true, "content-transfer-encoding": true,
	"content-disposition": true, "content-id": true,
}

// Build renders a message as RFC 5322 with CRLF line endings.
//
// The structure follows what mail clients expect: multipart/mixed wrapping a
// multipart/alternative when there are both bodies and attachments, and the
// simplest form that fits otherwise.
func Build(m *Message) ([]byte, error) {
	if m.From == "" {
		return nil, fmt.Errorf("mailmime: message has no From address")
	}

	var h gomail.Header
	h.SetContentType("text/plain", nil) // replaced below; keeps the header ordered first

	from, err := parseAddressList(m.From)
	if err != nil {
		return nil, fmt.Errorf("mailmime: From: %w", err)
	}
	h.SetAddressList("From", from)
	if err := setAddrs(&h, "To", m.To); err != nil {
		return nil, err
	}
	if err := setAddrs(&h, "Cc", m.Cc); err != nil {
		return nil, err
	}
	// Bcc is deliberately not written: the stored copy of a sent message must
	// not leak blind recipients to anyone the message is forwarded to.
	if err := setAddrs(&h, "Reply-To", m.ReplyTo); err != nil {
		return nil, err
	}

	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}
	h.SetDate(date)
	h.SetSubject(m.Subject)

	msgID := m.MessageID
	if msgID == "" {
		msgID = GenerateMessageID(domainOf(m.From))
	}
	h.Set("Message-Id", msgID)

	for k, v := range m.Headers {
		if headersNotCopied[strings.ToLower(k)] || v == "" {
			continue
		}
		h.Set(k, v)
	}

	var buf bytes.Buffer
	if err := writeBody(&buf, &h, m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeBody(buf *bytes.Buffer, h *gomail.Header, m *Message) error {
	text, html := m.Text, m.HTML
	if text == "" && html == "" {
		text = ""
	}

	switch {
	case len(m.Attachments) > 0:
		mw, err := gomail.CreateWriter(buf, *h)
		if err != nil {
			return err
		}
		if err := writeAlternative(mw, text, html); err != nil {
			return err
		}
		for _, a := range m.Attachments {
			if err := writeAttachment(mw, a); err != nil {
				return err
			}
		}
		return mw.Close()

	case html != "" && text != "":
		mw, err := gomail.CreateWriter(buf, *h)
		if err != nil {
			return err
		}
		if err := writeAlternative(mw, text, html); err != nil {
			return err
		}
		return mw.Close()

	default:
		contentType := "text/plain"
		body := text
		if html != "" {
			contentType, body = "text/html", html
		}
		h.SetContentType(contentType, map[string]string{"charset": "utf-8"})
		w, err := gomail.CreateSingleInlineWriter(buf, *h)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, body); err != nil {
			return err
		}
		return w.Close()
	}
}

func writeAlternative(mw *gomail.Writer, text, html string) error {
	iw, err := mw.CreateInline()
	if err != nil {
		return err
	}
	// text/plain first: RFC 2046 orders alternatives least to most faithful,
	// and clients pick the last one they can render.
	parts := []struct{ typ, body string }{}
	if text != "" || html == "" {
		parts = append(parts, struct{ typ, body string }{"text/plain", text})
	}
	if html != "" {
		parts = append(parts, struct{ typ, body string }{"text/html", html})
	}
	for _, p := range parts {
		var ph gomail.InlineHeader
		ph.SetContentType(p.typ, map[string]string{"charset": "utf-8"})
		w, err := iw.CreatePart(ph)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, p.body); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
	}
	return iw.Close()
}

func writeAttachment(mw *gomail.Writer, a Attachment) error {
	var ah gomail.AttachmentHeader
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	// Split any parameters the API handed back with the type.
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mediaType, params = "application/octet-stream", nil
	}
	if params == nil {
		params = map[string]string{}
	}
	ah.SetContentType(mediaType, params)
	if a.Filename != "" {
		ah.SetFilename(a.Filename)
	}
	if a.ContentID != "" {
		ah.Set("Content-Id", ensureAngled(a.ContentID))
		ah.Set("Content-Disposition", "inline")
	}
	w, err := mw.CreateAttachment(ah)
	if err != nil {
		return err
	}
	if _, err := w.Write(a.Data); err != nil {
		return err
	}
	return w.Close()
}

func setAddrs(h *gomail.Header, field string, addrs []string) error {
	list, err := parseAddressList(strings.Join(addrs, ", "))
	if err != nil {
		return fmt.Errorf("mailmime: %s: %w", field, err)
	}
	if len(list) == 0 {
		return nil
	}
	h.SetAddressList(field, list)
	return nil
}

// parseAddressList is lenient: Resend hands back addresses in several shapes
// and a single malformed recipient must not cost us the whole message. An
// address that will not parse is kept verbatim as the address part.
func parseAddressList(s string) ([]*gomail.Address, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parsed, err := mail.ParseAddressList(s)
	if err == nil {
		out := make([]*gomail.Address, 0, len(parsed))
		for _, a := range parsed {
			out = append(out, &gomail.Address{Name: a.Name, Address: a.Address})
		}
		return out, nil
	}
	var out []*gomail.Address
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if a, err := mail.ParseAddress(part); err == nil {
			out = append(out, &gomail.Address{Name: a.Name, Address: a.Address})
			continue
		}
		out = append(out, &gomail.Address{Address: part})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cannot parse %q", s)
	}
	return out, nil
}

// GenerateMessageID returns a fresh RFC 5322 Message-ID for the domain.
func GenerateMessageID(domain string) string {
	if domain == "" {
		domain = "ferry.local"
	}
	return "<" + uuid.NewString() + "@" + domain + ">"
}

func domainOf(addr string) string {
	if a, err := mail.ParseAddress(addr); err == nil {
		addr = a.Address
	}
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return strings.Trim(addr[i+1:], "> ")
	}
	return ""
}

func ensureAngled(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if !strings.HasPrefix(id, "<") {
		id = "<" + id
	}
	if !strings.HasSuffix(id, ">") {
		id += ">"
	}
	return id
}
