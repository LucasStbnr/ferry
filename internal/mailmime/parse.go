package mailmime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"strings"
	"time"

	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
)

// maxBodyText caps how much decoded text a single message contributes to the
// search index, so one enormous message cannot blow up memory during a sync.
const maxBodyText = 1 << 20

// Index is the searchable and displayable summary of a stored message. The
// store keeps these columns so IMAP SEARCH and ENVELOPE never have to re-parse
// every blob.
type Index struct {
	MessageID string
	Subject   string
	From      string
	To        string
	Date      time.Time
	Text      string
}

// Parse extracts index fields from a complete RFC 5322 message. It is
// deliberately forgiving: a message that will not parse still yields whatever
// headers were readable, because refusing to store it would lose mail.
func Parse(raw []byte) Index {
	var idx Index

	ent, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil && ent == nil {
		// Inbound mail is not always well-formed, and a message we cannot
		// parse is still a message the user must be able to find. Fall back
		// to a line-by-line header scan rather than returning nothing.
		return parseLenient(raw)
	}
	h := gomail.Header{Header: ent.Header}

	if v, err := h.Text("Subject"); err == nil {
		idx.Subject = v
	} else {
		idx.Subject = h.Get("Subject")
	}
	idx.MessageID = strings.TrimSpace(h.Get("Message-Id"))
	idx.From = addressListString(h, "From")
	idx.To = addressListString(h, "To")
	if cc := addressListString(h, "Cc"); cc != "" {
		idx.To = strings.TrimSpace(idx.To + " " + cc)
	}
	if t, err := h.Date(); err == nil {
		idx.Date = t
	}
	idx.Text = bodyText(ent, 0)
	return idx
}

// parseLenient extracts what it can from a message go-message rejected. It
// reads headers until the first blank line, tolerating junk lines, and treats
// the remainder as body text.
func parseLenient(raw []byte) Index {
	var idx Index
	text := string(raw)
	headerEnd := strings.Index(text, "\r\n\r\n")
	sep := 4
	if headerEnd < 0 {
		headerEnd = strings.Index(text, "\n\n")
		sep = 2
	}
	head, body := text, ""
	if headerEnd >= 0 {
		head, body = text[:headerEnd], text[headerEnd+sep:]
	}

	fields := map[string]string{}
	var lastKey string
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && lastKey != "" {
			fields[lastKey] += " " + strings.TrimSpace(line)
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			lastKey = ""
			continue // junk line: skip it and keep going
		}
		lastKey = strings.ToLower(strings.TrimSpace(line[:colon]))
		fields[lastKey] = strings.TrimSpace(line[colon+1:])
	}

	idx.Subject = DecodeHeader(fields["subject"])
	idx.MessageID = fields["message-id"]
	idx.From = DecodeHeader(fields["from"])
	idx.To = strings.TrimSpace(DecodeHeader(fields["to"]) + " " + DecodeHeader(fields["cc"]))
	if t, err := mail.ParseDate(fields["date"]); err == nil {
		idx.Date = t
	}
	if len(body) > maxBodyText {
		body = body[:maxBodyText]
	}
	idx.Text = body
	return idx
}

func addressListString(h gomail.Header, field string) string {
	list, err := h.AddressList(field)
	if err != nil || len(list) == 0 {
		return strings.TrimSpace(h.Get(field))
	}
	parts := make([]string, 0, len(list))
	for _, a := range list {
		if a.Name != "" {
			parts = append(parts, a.Name+" <"+a.Address+">")
			continue
		}
		parts = append(parts, a.Address)
	}
	return strings.Join(parts, ", ")
}

// bodyText walks an entity and concatenates its readable text, preferring
// text/plain. depth guards against a maliciously deep multipart nesting.
func bodyText(e *gomessage.Entity, depth int) string {
	if e == nil || depth > 10 {
		return ""
	}
	if mr := e.MultipartReader(); mr != nil {
		var (
			plain strings.Builder
			html  strings.Builder
		)
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				break
			}
			typ, _, _ := part.Header.ContentType()
			text := bodyText(part, depth+1)
			if text == "" {
				continue
			}
			if typ == "text/html" {
				appendCapped(&html, text)
				continue
			}
			appendCapped(&plain, text)
		}
		if plain.Len() > 0 {
			return plain.String()
		}
		return html.String()
	}

	typ, _, err := e.Header.ContentType()
	if err != nil {
		typ = "text/plain"
	}
	if !strings.HasPrefix(typ, "text/") {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(e.Body, maxBodyText))
	if err != nil {
		return ""
	}
	if typ == "text/html" {
		return stripTags(string(b))
	}
	return string(b)
}

func appendCapped(sb *strings.Builder, s string) {
	if sb.Len() >= maxBodyText {
		return
	}
	if sb.Len() > 0 {
		sb.WriteByte('\n')
	}
	if sb.Len()+len(s) > maxBodyText {
		s = s[:maxBodyText-sb.Len()]
	}
	sb.WriteString(s)
}

// stripTags reduces HTML to its text, which is all the search index needs.
// It drops the contents of script and style elements so that CSS selectors and
// JavaScript identifiers do not become search terms.
func stripTags(s string) string {
	var out strings.Builder
	out.Grow(len(s) / 2)
	for i := 0; i < len(s); {
		c := s[i]
		if c != '<' {
			out.WriteByte(c)
			i++
			continue
		}
		rest := s[i:]
		for _, tag := range []string{"script", "style"} {
			if hasTagPrefix(rest, tag) {
				if end := indexCloseTag(s[i:], tag); end >= 0 {
					i += end
					goto next
				}
				return out.String()
			}
		}
		if j := strings.IndexByte(s[i:], '>'); j >= 0 {
			i += j + 1
			out.WriteByte(' ')
			continue
		}
		return out.String()
	next:
	}
	return out.String()
}

func hasTagPrefix(s, tag string) bool {
	if len(s) < len(tag)+1 || s[0] != '<' {
		return false
	}
	if !strings.EqualFold(s[1:1+len(tag)], tag) {
		return false
	}
	next := s[1+len(tag):]
	return next == "" || next[0] == '>' || next[0] == ' ' || next[0] == '\t' || next[0] == '\n' || next[0] == '/'
}

func indexCloseTag(s, tag string) int {
	closing := "</" + tag
	lower := strings.ToLower(s)
	j := strings.Index(lower, closing)
	if j < 0 {
		return -1
	}
	k := strings.IndexByte(s[j:], '>')
	if k < 0 {
		return -1
	}
	return j + k + 1
}

// Submission is a message a client handed to the SMTP server, reduced to the
// pieces Resend's send API accepts.
type Submission struct {
	From        string
	To          []string
	Cc          []string
	Bcc         []string
	ReplyTo     []string
	Subject     string
	Text        string
	HTML        string
	MessageID   string
	InReplyTo   string
	References  string
	Date        time.Time
	Attachments []Attachment
	// Raw is the message exactly as submitted, stored in Sent so the user's
	// own copy is byte-for-byte what they composed.
	Raw []byte
	// Unsupported names content Resend's structured API cannot carry, such as
	// an S/MIME signature. The SMTP server refuses rather than silently
	// sending a message with parts missing.
	Unsupported []string
}

// ParseSubmission reads a complete submitted message.
func ParseSubmission(r io.Reader, maxBytes int64) (*Submission, error) {
	limited := io.LimitReader(r, maxBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("mailmime: read submission: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("mailmime: message is larger than %d bytes", maxBytes)
	}

	ent, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil && ent == nil {
		return nil, fmt.Errorf("mailmime: parse submission: %w", err)
	}
	h := gomail.Header{Header: ent.Header}

	sub := &Submission{Raw: raw}
	sub.From = firstAddress(h, "From")
	sub.To = addressStrings(h, "To")
	sub.Cc = addressStrings(h, "Cc")
	sub.Bcc = addressStrings(h, "Bcc")
	sub.ReplyTo = addressStrings(h, "Reply-To")
	if v, err := h.Text("Subject"); err == nil {
		sub.Subject = v
	} else {
		sub.Subject = h.Get("Subject")
	}
	sub.MessageID = strings.TrimSpace(h.Get("Message-Id"))
	sub.InReplyTo = strings.TrimSpace(h.Get("In-Reply-To"))
	sub.References = strings.TrimSpace(h.Get("References"))
	if t, err := h.Date(); err == nil {
		sub.Date = t
	}

	if err := collectParts(ent, sub, 0); err != nil {
		return nil, err
	}
	return sub, nil
}

// unsupportedTypes are content types whose meaning is destroyed by being
// re-encoded through a structured JSON API.
var unsupportedTypes = map[string]string{
	"application/pkcs7-signature":   "S/MIME signature",
	"application/x-pkcs7-signature": "S/MIME signature",
	"application/pkcs7-mime":        "S/MIME encrypted message",
	"application/pgp-signature":     "PGP signature",
	"application/pgp-encrypted":     "PGP encrypted message",
}

func collectParts(e *gomessage.Entity, sub *Submission, depth int) error {
	if depth > 10 {
		return errors.New("mailmime: multipart nesting is too deep")
	}
	typ, _, _ := e.Header.ContentType()
	if note, bad := unsupportedTypes[typ]; bad {
		sub.Unsupported = appendUnique(sub.Unsupported, note)
		return nil
	}

	if mr := e.MultipartReader(); mr != nil {
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("mailmime: read part: %w", err)
			}
			if err := collectParts(part, sub, depth+1); err != nil {
				return err
			}
		}
	}

	disp, dispParams, _ := e.Header.ContentDisposition()
	filename := dispParams["filename"]
	if filename == "" {
		_, ctParams, _ := e.Header.ContentType()
		filename = ctParams["name"]
	}
	contentID := strings.Trim(e.Header.Get("Content-Id"), "<> ")
	isAttachment := disp == "attachment" || (filename != "" && disp != "inline") || contentID != ""

	body, err := io.ReadAll(e.Body)
	if err != nil {
		return fmt.Errorf("mailmime: read part body: %w", err)
	}

	switch {
	case isAttachment:
		if filename == "" {
			filename = "attachment"
		}
		sub.Attachments = append(sub.Attachments, Attachment{
			Filename:    filename,
			ContentType: typ,
			ContentID:   contentID,
			Data:        body,
		})
	case typ == "text/html":
		sub.HTML += string(body)
	case typ == "text/plain" || typ == "":
		sub.Text += string(body)
	default:
		// A non-text part with no disposition is still content the user meant
		// to send; carry it as an attachment rather than dropping it.
		sub.Attachments = append(sub.Attachments, Attachment{
			Filename:    "part",
			ContentType: typ,
			Data:        body,
		})
	}
	return nil
}

func firstAddress(h gomail.Header, field string) string {
	list, err := h.AddressList(field)
	if err != nil || len(list) == 0 {
		return strings.TrimSpace(h.Get(field))
	}
	if list[0].Name != "" {
		return (&mail.Address{Name: list[0].Name, Address: list[0].Address}).String()
	}
	return list[0].Address
}

func addressStrings(h gomail.Header, field string) []string {
	list, err := h.AddressList(field)
	if err != nil || len(list) == 0 {
		if v := strings.TrimSpace(h.Get(field)); v != "" && err != nil {
			return []string{v}
		}
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		if a.Name != "" {
			out = append(out, (&mail.Address{Name: a.Name, Address: a.Address}).String())
			continue
		}
		out = append(out, a.Address)
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

// AddressOnly strips any display name, giving the bare addr-spec.
func AddressOnly(addr string) string {
	if a, err := mail.ParseAddress(addr); err == nil {
		return a.Address
	}
	addr = strings.TrimSpace(addr)
	if i := strings.LastIndex(addr, "<"); i >= 0 {
		if j := strings.Index(addr[i:], ">"); j >= 0 {
			return addr[i+1 : i+j]
		}
	}
	return addr
}

// Domain returns the lower-cased domain of an address.
func Domain(addr string) string {
	a := AddressOnly(addr)
	if i := strings.LastIndex(a, "@"); i >= 0 {
		return strings.ToLower(a[i+1:])
	}
	return ""
}

// DecodeHeader decodes any RFC 2047 encoded-words in a raw header value.
func DecodeHeader(v string) string {
	dec := new(mime.WordDecoder)
	out, err := dec.DecodeHeader(v)
	if err != nil {
		return v
	}
	return out
}
