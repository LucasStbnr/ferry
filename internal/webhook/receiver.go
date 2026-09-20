package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/LucasStbnr/ferry/internal/mailmime"
	"github.com/LucasStbnr/ferry/internal/store"
)

// maxBodyBytes caps a webhook request. Resend's events are small; anything
// larger is either a mistake or an attempt to exhaust memory.
const maxBodyBytes = 1 << 20

// Event is the envelope Resend posts.
type Event struct {
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

// EmailData is the payload of the email.* events Ferry acts on.
type EmailData struct {
	ID        string   `json:"email_id"`
	From      string   `json:"from"`
	To        []string `json:"to"`
	Subject   string   `json:"subject"`
	CreatedAt string   `json:"created_at"`
	Bounce    *struct {
		Type    string `json:"type"`
		SubType string `json:"subType"`
		Message string `json:"message"`
	} `json:"bounce"`
}

// Event types Ferry understands.
const (
	TypeReceived   = "email.received"
	TypeBounced    = "email.bounced"
	TypeComplained = "email.complained"
	TypeDelivered  = "email.delivered"
)

// errUnknownAccount is returned when a request names, or matches, no account
// that has webhooks enabled.
var errUnknownAccount = errors.New("webhook: no account matched")

// Syncer is the part of the sync engine the receiver needs: a way to ask for
// an immediate poll.
type Syncer interface {
	Trigger()
	Account() string
}

// Options configure the receiver.
type Options struct {
	// Path is the URL path served, e.g. /webhooks/resend. An account name may
	// follow it, so /webhooks/resend/mysite routes to that account.
	Path   string
	DB     *store.DB
	Logger *slog.Logger
	// Notifier is told when a mailbox changes, to wake idling IMAP clients.
	Notifier func(account string, mailboxID int64)
}

// Receiver is the HTTP handler for Resend webhooks.
type Receiver struct {
	opts Options
	log  *slog.Logger

	mu        sync.RWMutex
	verifiers map[string]*Verifier // by account name
	syncers   map[string]Syncer

	// noticeMu serialises filing delivery notices. Resend retries an event it
	// believes failed, and those retries can overlap, so the "already filed?"
	// check and the append have to happen together or a bounce shows up
	// several times in the Inbox.
	noticeMu sync.Mutex
}

// New creates a receiver with no accounts registered.
func New(opts Options) *Receiver {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Path == "" {
		opts.Path = "/webhooks/resend"
	}
	return &Receiver{
		opts:      opts,
		log:       opts.Logger,
		verifiers: map[string]*Verifier{},
		syncers:   map[string]Syncer{},
	}
}

// Register enables webhooks for an account. Without a signing secret the
// account's events are rejected, which is the safe default.
func (r *Receiver) Register(account, signingSecret string, syncer Syncer) error {
	v, err := NewVerifier(signingSecret)
	if err != nil {
		return fmt.Errorf("webhook: account %s: %w", account, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.verifiers[account] = v
	if syncer != nil {
		r.syncers[account] = syncer
	}
	return nil
}

// Unregister stops accepting events for an account.
func (r *Receiver) Unregister(account string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.verifiers, account)
	delete(r.syncers, account)
}

// Accounts lists the accounts with webhooks enabled.
func (r *Receiver) Accounts() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.verifiers))
	for name := range r.verifiers {
		out = append(out, name)
	}
	return out
}

// Handler returns the HTTP handler to mount.
func (r *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(strings.TrimSuffix(r.opts.Path, "/")+"/", r.serve)
	mux.HandleFunc(strings.TrimSuffix(r.opts.Path, "/"), r.serve)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

func (r *Receiver) serve(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, maxBodyBytes+1))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	account := r.routeAccount(req.URL.Path)
	verified, err := r.verify(account, req.Header, body)
	if err != nil {
		// Nothing from the request reaches the log. This runs before any
		// signature has been checked, so the path, the headers and the body
		// are all attacker-controlled, and the error built from them carries
		// header bytes with it. Only rejectionReason's fixed strings are
		// logged, which makes it structurally impossible to write chosen
		// bytes into the log or to flood it from outside.
		//
		// The reason is never returned to the caller either: someone probing
		// the endpoint learns nothing about which accounts exist or why a
		// signature failed.
		r.log.Warn("rejected webhook", "reason", rejectionReason(err))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "malformed event", http.StatusBadRequest)
		return
	}

	// Answer before doing the work: Resend retries on a slow response, and a
	// duplicate event would cost an extra sync for nothing.
	w.WriteHeader(http.StatusNoContent)

	go r.handle(context.WithoutCancel(req.Context()), verified, &event)
}

// routeAccount reads the account name from the path suffix, if any.
func (r *Receiver) routeAccount(path string) string {
	base := strings.TrimSuffix(r.opts.Path, "/")
	rest := strings.Trim(strings.TrimPrefix(path, base), "/")
	if rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	return rest
}

// verify finds the account whose secret signs this request. With an account in
// the path only that one is tried; otherwise each registered secret is tried,
// which is what lets one endpoint serve several accounts.
func (r *Receiver) verify(account string, h http.Header, body []byte) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if account != "" {
		v, ok := r.verifiers[account]
		if !ok {
			return "", errUnknownAccount
		}
		if err := v.Verify(h, body); err != nil {
			return "", err
		}
		return account, nil
	}

	if len(r.verifiers) == 0 {
		return "", errUnknownAccount
	}
	var lastErr error
	for name, v := range r.verifiers {
		err := v.Verify(h, body)
		if err == nil {
			return name, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func (r *Receiver) handle(ctx context.Context, account string, event *Event) {
	// account is a name Ferry registered, so it is ours. The event type comes
	// out of the payload, so it is reduced to one of the known constants.
	log := r.log.With("account", account, "event", eventLabel(event.Type))

	switch event.Type {
	case TypeReceived:
		r.mu.RLock()
		syncer := r.syncers[account]
		r.mu.RUnlock()
		if syncer == nil {
			log.Debug("no syncer registered; the next poll will pick this up")
			return
		}
		log.Debug("new mail announced, syncing now")
		syncer.Trigger()

	case TypeBounced, TypeComplained:
		if err := r.fileNotice(ctx, account, event); err != nil {
			log.Error("could not file delivery notice", "error", err)
		}

	default:
		// Delivery and open events carry no information Ferry can show in a
		// mail client, so they are acknowledged and dropped.
		log.Debug("ignoring event")
	}
}

// fileNotice turns a bounce or complaint into a message in the Inbox. Resend
// shows these in its dashboard; a bridge that hid them would let a failed
// delivery pass unnoticed, which is the one thing a sender must never miss.
func (r *Receiver) fileNotice(ctx context.Context, account string, event *Event) error {
	var data EmailData
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return fmt.Errorf("webhook: parse %s payload: %w", event.Type, err)
	}

	r.noticeMu.Lock()
	defer r.noticeMu.Unlock()

	acct, err := r.opts.DB.AccountByName(ctx, account)
	if err != nil {
		return err
	}
	as := r.opts.DB.Account(acct)

	// One notice per event: a Resend retry must not produce a second copy.
	noticeID := noticeMessageID(event.Type, data.ID)
	if existing, err := as.FindByMessageID(ctx, noticeID); err == nil && existing != nil {
		return nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	subject, body := noticeText(event.Type, &data)
	when := event.CreatedAt
	if when.IsZero() {
		when = time.Now()
	}

	from := "mailer-daemon@" + firstDomain(acct.Domains, "ferry.local")
	raw, err := mailmime.Build(&mailmime.Message{
		MessageID: noticeID,
		From:      "Mail Delivery Subsystem <" + from + ">",
		To:        []string{acct.Address},
		Subject:   subject,
		Date:      when,
		Text:      body,
		Headers: map[string]string{
			"Auto-Submitted": "auto-replied",
			"X-Ferry-Event":  event.Type,
		},
	})
	if err != nil {
		return err
	}

	mbox, err := as.Mailbox(ctx, store.Inbox)
	if err != nil {
		return err
	}
	if _, err := as.Append(ctx, mbox.ID, &store.NewMessage{
		Raw:          raw,
		MessageID:    noticeID,
		InternalDate: when,
		SentDate:     when,
		Subject:      subject,
		From:         from,
		To:           acct.Address,
		SearchText:   body,
		Flags:        []string{`\Flagged`}, // a failed delivery deserves attention
	}); err != nil {
		return err
	}
	if r.opts.Notifier != nil {
		r.opts.Notifier(account, mbox.ID)
	}
	r.log.Info("filed delivery notice",
		"account", account, "event", eventLabel(event.Type), "email_id", safeID(data.ID))
	return nil
}

// rejectionReason maps a verification failure onto one of a fixed set of
// strings. The error itself is built from request headers, so returning it to
// the logger would put attacker-chosen bytes there; a closed set cannot.
func rejectionReason(err error) string {
	switch {
	case errors.Is(err, ErrNoSignature):
		return "no signature headers"
	case errors.Is(err, ErrStaleRequest):
		return "timestamp outside the tolerance window"
	case errors.Is(err, ErrBadSignature):
		return "signature did not match"
	case errors.Is(err, ErrMalformedHead):
		return "malformed signature headers"
	case errors.Is(err, errUnknownAccount):
		return "no account with a signing secret matched"
	default:
		return "verification failed"
	}
}

// eventLabel reduces an event type from the payload to a known constant, so
// only strings from this file are ever logged.
func eventLabel(t string) string {
	switch t {
	case TypeReceived, TypeBounced, TypeComplained, TypeDelivered:
		return t
	default:
		return "other"
	}
}

// safeIDPattern is what a Resend identifier looks like. An id that does not
// match is replaced rather than logged, so the log holds either a real
// identifier or a marker, never arbitrary bytes.
var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func safeID(id string) string {
	if safeIDPattern.MatchString(id) {
		return id
	}
	return "(invalid)"
}

func noticeMessageID(eventType, emailID string) string {
	return "<" + strings.ReplaceAll(eventType, ".", "-") + "-" + emailID + "@ferry.local>"
}

func noticeText(eventType string, d *EmailData) (subject, body string) {
	recipients := strings.Join(d.To, ", ")
	var sb strings.Builder

	switch eventType {
	case TypeBounced:
		subject = "Undelivered mail: " + orPlaceholder(d.Subject, "(no subject)")
		fmt.Fprintf(&sb, "Your message was not delivered to %s.\n\n", orPlaceholder(recipients, "the recipient"))
		if d.Bounce != nil {
			if d.Bounce.Type != "" {
				fmt.Fprintf(&sb, "Bounce type: %s", d.Bounce.Type)
				if d.Bounce.SubType != "" {
					fmt.Fprintf(&sb, " (%s)", d.Bounce.SubType)
				}
				sb.WriteString("\n")
			}
			if d.Bounce.Message != "" {
				fmt.Fprintf(&sb, "Reason: %s\n", d.Bounce.Message)
			}
			if strings.EqualFold(d.Bounce.Type, "Permanent") {
				sb.WriteString("\nThis address is now on Resend's suppression list; " +
					"further mail to it will not be attempted.\n")
			}
		}
	case TypeComplained:
		subject = "Spam complaint: " + orPlaceholder(d.Subject, "(no subject)")
		fmt.Fprintf(&sb, "%s marked your message as spam.\n\n", orPlaceholder(recipients, "A recipient"))
		sb.WriteString("Resend suppresses further mail to this address. " +
			"Repeated complaints affect the sending reputation of the whole domain.\n")
	}

	fmt.Fprintf(&sb, "\nOriginal subject: %s\n", orPlaceholder(d.Subject, "(no subject)"))
	if d.From != "" {
		fmt.Fprintf(&sb, "Sent from: %s\n", d.From)
	}
	if d.ID != "" {
		fmt.Fprintf(&sb, "Resend message id: %s\n", d.ID)
	}
	sb.WriteString("\nThis notice was generated by Ferry from a Resend webhook.\n")
	return subject, sb.String()
}

func orPlaceholder(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func firstDomain(domains []string, fallback string) string {
	if len(domains) > 0 {
		return domains[0]
	}
	return fallback
}
