package resend

import (
	"encoding/json"
	"time"
)

// Domain is a Resend sending/receiving domain.
type Domain struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Status       string             `json:"status"`
	Capabilities DomainCapabilities `json:"capabilities"`
}

// DomainCapabilities tells what a domain is enabled for.
type DomainCapabilities struct {
	Sending   string `json:"sending"`
	Receiving string `json:"receiving"`
}

// CanSend reports whether mail may be sent from the domain.
func (d Domain) CanSend() bool {
	if d.Capabilities.Sending != "" {
		return d.Capabilities.Sending == "enabled" && d.Status == "verified"
	}
	return d.Status == "verified"
}

// Summary is one row of a list endpoint (received or sent).
type Summary struct {
	ID          string     `json:"id"`
	From        string     `json:"from"`
	To          Addrs      `json:"to"`
	Cc          Addrs      `json:"cc"`
	Bcc         Addrs      `json:"bcc"`
	Subject     string     `json:"subject"`
	MessageID   string     `json:"message_id"`
	CreatedAt   time.Time  `json:"created_at"`
	Attachments []Attached `json:"attachments"`
}

// Attached is attachment metadata.
type Attached struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Disposition string `json:"content_disposition"`
	ContentID   string `json:"content_id"`
	Size        int64  `json:"size"`
}

// Raw points at the original RFC 5322 message. The URL is signed and expires.
type Raw struct {
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Email is a full received or sent message.
type Email struct {
	ID          string            `json:"id"`
	MessageID   string            `json:"message_id"`
	From        string            `json:"from"`
	To          Addrs             `json:"to"`
	Cc          Addrs             `json:"cc"`
	Bcc         Addrs             `json:"bcc"`
	ReplyTo     Addrs             `json:"reply_to"`
	ReceivedFor Addrs             `json:"received_for"`
	Subject     string            `json:"subject"`
	HTML        string            `json:"html"`
	HTMLFormat  string            `json:"html_format"`
	Text        string            `json:"text"`
	Headers     map[string]string `json:"headers"`
	CreatedAt   time.Time         `json:"created_at"`
	Raw         *Raw              `json:"raw"`
	Attachments []Attached        `json:"attachments"`
}

// AttachmentInfo is a single attachment with its temporary download URL.
type AttachmentInfo struct {
	Attached
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Page is one page of a cursor-paginated listing, newest first.
type Page struct {
	HasMore bool
	Items   []Summary
}

// ListParams selects a page. After is the cursor: the page starts right after
// (older than) this ID. Zero Limit means the API maximum of 100.
type ListParams struct {
	After string
	Limit int
}

// SendAttachment is an attachment in a send request (base64 content).
type SendAttachment struct {
	Filename    string `json:"filename"`
	Content     string `json:"content"`
	ContentType string `json:"content_type,omitempty"`
	ContentID   string `json:"content_id,omitempty"`
}

// SendRequest is the JSON body of POST /emails.
type SendRequest struct {
	From        string            `json:"from"`
	To          []string          `json:"to"`
	Cc          []string          `json:"cc,omitempty"`
	Bcc         []string          `json:"bcc,omitempty"`
	ReplyTo     []string          `json:"reply_to,omitempty"`
	Subject     string            `json:"subject"`
	HTML        string            `json:"html,omitempty"`
	Text        string            `json:"text,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Attachments []SendAttachment  `json:"attachments,omitempty"`
}

// Limits imposed by the Resend send API.
const (
	MaxRecipients      = 50
	MaxAttachmentBytes = 40 << 20
)

// Addrs is a list of addresses that Resend sometimes encodes as a single
// string, sometimes as an array and sometimes as null.
type Addrs []string

// UnmarshalJSON implements json.Unmarshaler.
func (a *Addrs) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if one != "" {
			*a = Addrs{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}
