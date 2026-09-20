// Package smtpd accepts mail from Apple Mail and hands it to Resend.
//
// Submission is synchronous: the SMTP transaction does not succeed until
// Resend has accepted the message. That is deliberate. A queue behind the
// bridge would mean Mail shows a message as sent while it is still in limbo,
// and the user would have no way to see or retry it. Returning the real error
// on the SMTP transaction puts the failure where Mail already knows how to
// show it: the Outbox.
package smtpd

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/LucasStbnr/ferry/internal/mailmime"
	"github.com/LucasStbnr/ferry/internal/resend"
)

// Limits imposed by the Resend send API, restated here so the error messages
// can name them before a request is made.
const (
	MaxRecipients      = resend.MaxRecipients
	MaxAttachmentBytes = resend.MaxAttachmentBytes
)

// buildSendRequest converts a parsed submission into a Resend send payload.
//
// The envelope recipients are authoritative: Mail strips Bcc from the message
// it submits and lists those recipients only in RCPT TO, so a Bcc would be
// silently dropped if the payload were built from headers alone.
func buildSendRequest(sub *mailmime.Submission, envelopeRcpts []string) (*resend.SendRequest, error) {
	if len(sub.Unsupported) > 0 {
		return nil, &SendError{
			Permanent: true,
			Code:      550,
			Enhanced:  [3]int{5, 6, 0},
			Message: "Ferry cannot send this message through Resend: it contains " +
				strings.Join(sub.Unsupported, " and ") +
				". Resend's API takes structured content, not signed or encrypted MIME.",
		}
	}

	headerRcpts := map[string]bool{}
	for _, list := range [][]string{sub.To, sub.Cc} {
		for _, a := range list {
			headerRcpts[strings.ToLower(mailmime.AddressOnly(a))] = true
		}
	}

	// Anything in the envelope but not in To or Cc is a Bcc.
	var bcc []string
	for _, r := range envelopeRcpts {
		addr := mailmime.AddressOnly(r)
		if !headerRcpts[strings.ToLower(addr)] {
			bcc = append(bcc, addr)
		}
	}
	for _, b := range sub.Bcc {
		addr := mailmime.AddressOnly(b)
		if !headerRcpts[strings.ToLower(addr)] && !contains(bcc, addr) {
			bcc = append(bcc, addr)
		}
	}

	req := &resend.SendRequest{
		From:    sub.From,
		To:      sub.To,
		Cc:      sub.Cc,
		Bcc:     bcc,
		ReplyTo: sub.ReplyTo,
		Subject: sub.Subject,
		Text:    sub.Text,
		HTML:    sub.HTML,
	}
	if len(req.To) == 0 {
		// A message addressed only via Bcc still needs a To for the API.
		req.To = bcc
		req.Bcc = nil
	}
	if len(req.To) == 0 {
		return nil, &SendError{
			Permanent: true, Code: 554, Enhanced: [3]int{5, 1, 3},
			Message: "The message has no recipients.",
		}
	}

	total := len(req.To) + len(req.Cc) + len(req.Bcc)
	if total > MaxRecipients {
		return nil, &SendError{
			Permanent: true, Code: 452, Enhanced: [3]int{5, 5, 3},
			Message: fmt.Sprintf("Resend accepts at most %d recipients per message; this one has %d.",
				MaxRecipients, total),
		}
	}

	// Threading headers must survive, or every reply starts a new thread in
	// the recipient's client.
	headers := map[string]string{}
	if sub.InReplyTo != "" {
		headers["In-Reply-To"] = sub.InReplyTo
	}
	if sub.References != "" {
		headers["References"] = sub.References
	}
	if sub.MessageID != "" {
		headers["Message-Id"] = sub.MessageID
	}
	if len(headers) > 0 {
		req.Headers = headers
	}

	var attachmentBytes int64
	for _, a := range sub.Attachments {
		attachmentBytes += int64(len(a.Data))
		req.Attachments = append(req.Attachments, resend.SendAttachment{
			Filename:    a.Filename,
			Content:     base64.StdEncoding.EncodeToString(a.Data),
			ContentType: a.ContentType,
			ContentID:   a.ContentID,
		})
	}
	if attachmentBytes > MaxAttachmentBytes {
		return nil, &SendError{
			Permanent: true, Code: 552, Enhanced: [3]int{5, 3, 4},
			Message: fmt.Sprintf("Attachments total %.1f MB; Resend's limit is %d MB.",
				float64(attachmentBytes)/(1<<20), MaxAttachmentBytes>>20),
		}
	}

	if req.Text == "" && req.HTML == "" {
		// Resend rejects a message with neither body; an empty text part is
		// closer to the user's intent than a refusal.
		req.Text = " "
	}
	return req, nil
}

// SendError is a failure to map onto an SMTP reply. Permanent failures get a
// 5xx so Mail stops retrying and shows the message; temporary ones get a 4xx.
type SendError struct {
	Permanent bool
	Code      int
	Enhanced  [3]int
	Message   string
	Cause     error
}

func (e *SendError) Error() string { return e.Message }
func (e *SendError) Unwrap() error { return e.Cause }

// classify turns a Resend API error into an SMTP-shaped error whose text is
// worth showing to someone looking at a stuck message in Mail.
func classify(err error) *SendError {
	var apiErr *resend.APIError
	if !errors.As(err, &apiErr) {
		return &SendError{
			Permanent: false, Code: 451, Enhanced: [3]int{4, 4, 1},
			Message: "Could not reach Resend: " + err.Error(),
			Cause:   err,
		}
	}

	switch {
	case resend.IsQuota(err):
		return &SendError{
			Permanent: true, Code: 552, Enhanced: [3]int{5, 2, 2},
			Message: "Resend's sending quota is used up (" + apiErr.Message +
				"). The message was not sent; it will send once the quota resets or the plan is upgraded.",
			Cause: err,
		}
	case resend.IsRateLimited(err):
		return &SendError{
			Permanent: false, Code: 451, Enhanced: [3]int{4, 4, 5},
			Message: "Resend is rate limiting this account; try again in a moment.",
			Cause:   err,
		}
	case resend.IsAuth(err):
		return &SendError{
			Permanent: true, Code: 550, Enhanced: [3]int{5, 7, 0},
			Message: "Resend rejected Ferry's API key for this account. Run `ferry account add` again with a valid key.",
			Cause:   err,
		}
	case apiErr.Status == 422 || apiErr.Status == 400:
		return &SendError{
			Permanent: true, Code: 550, Enhanced: [3]int{5, 6, 0},
			Message: "Resend rejected the message: " + apiErr.Message,
			Cause:   err,
		}
	case apiErr.Status >= 500:
		return &SendError{
			Permanent: false, Code: 451, Enhanced: [3]int{4, 4, 1},
			Message: "Resend is having trouble (" + apiErr.Message + "); try again shortly.",
			Cause:   err,
		}
	}
	return &SendError{
		Permanent: true, Code: 550, Enhanced: [3]int{5, 0, 0},
		Message: "Resend rejected the message: " + apiErr.Error(),
		Cause:   err,
	}
}

// idempotencyKey derives a stable key from the message itself, so a retry
// after a timeout cannot send the message twice. Resend honours the key for
// 24 hours, which comfortably covers Mail's retry behaviour.
func idempotencyKey(sub *mailmime.Submission) string {
	if sub.MessageID != "" {
		return "ferry-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(sub.MessageID)).String()
	}
	return "ferry-" + uuid.NewSHA1(uuid.NameSpaceOID, sub.Raw).String()
}

func contains(list []string, v string) bool {
	for _, e := range list {
		if strings.EqualFold(e, v) {
			return true
		}
	}
	return false
}

// checkFromDomain refuses a From address on a domain the account cannot send
// from. Resend would reject it anyway; failing here gives a clearer message
// and does not spend a request.
func checkFromDomain(from string, domains []string) error {
	domain := mailmime.Domain(from)
	if domain == "" {
		return &SendError{
			Permanent: true, Code: 550, Enhanced: [3]int{5, 7, 1},
			Message: "The From address is not a valid email address.",
		}
	}
	if len(domains) == 0 {
		return &SendError{
			Permanent: true, Code: 550, Enhanced: [3]int{5, 7, 1},
			Message: "This account has no verified sending domain in Resend. Verify a domain, then run `ferry account refresh`.",
		}
	}
	for _, d := range domains {
		if strings.EqualFold(d, domain) {
			return nil
		}
	}
	return &SendError{
		Permanent: true, Code: 550, Enhanced: [3]int{5, 7, 1},
		Message: fmt.Sprintf("This account cannot send from %s. Its verified domains are: %s.",
			domain, strings.Join(domains, ", ")),
	}
}

// send submits the message and returns the Resend email id.
func send(ctx context.Context, client *resend.Client, req *resend.SendRequest, key string) (string, error) {
	id, err := client.Send(ctx, req, key)
	if err != nil {
		return "", classify(err)
	}
	return id, nil
}
