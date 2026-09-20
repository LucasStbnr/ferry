// Package mailsync keeps the local store in step with a Resend account.
//
// Resend is an append-only archive: it has no read state, no folders and no
// delete, and its raw-message and attachment URLs are signed and expire. So
// the sync is one-directional and greedy: every message is fetched and stored
// in full the first time it is seen, and local decisions (flags, moves,
// deletes) are never written back.
//
// Two passes share one cursor-paginated, newest-first listing:
//
//	incremental  from the newest row down until a message already stored
//	backfill     from the oldest row fetched so far, further into the past
//
// Both are resumable: the cursors live in the database, so an interrupted
// first-run backfill of a large history picks up exactly where it stopped.
package mailsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/LucasStbnr/ferry/internal/mailmime"
	"github.com/LucasStbnr/ferry/internal/resend"
	"github.com/LucasStbnr/ferry/internal/store"
)

// Notifier is told when a mailbox gained messages, so the IMAP server can wake
// an idling client. It is optional.
type Notifier interface {
	MailboxChanged(account string, mailboxID int64)
}

// Options configure a Syncer.
type Options struct {
	// PageSize is how many list rows to request at a time (max 100).
	PageSize int
	// MaxMessageBytes caps a single stored message.
	MaxMessageBytes int64
	// Interval between automatic polls. Zero disables the poller.
	Interval time.Duration
	// Logger receives progress and per-message failures.
	Logger *slog.Logger
	// Notifier is woken when a mailbox changes.
	Notifier Notifier
	// Clock is overridable in tests.
	Clock func() time.Time
}

func (o *Options) setDefaults() {
	if o.PageSize <= 0 || o.PageSize > 100 {
		o.PageSize = 100
	}
	if o.MaxMessageBytes <= 0 {
		o.MaxMessageBytes = 64 << 20
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
}

// Syncer synchronises one account.
type Syncer struct {
	api   *resend.Client
	store *store.AccountStore
	opts  Options

	// trigger carries on-demand sync requests to the poller loop.
	trigger chan struct{}
}

// New creates a syncer for one account.
func New(api *resend.Client, as *store.AccountStore, opts Options) *Syncer {
	opts.setDefaults()
	return &Syncer{
		api:     api,
		store:   as,
		opts:    opts,
		trigger: make(chan struct{}, 1),
	}
}

// Result reports what one sync pass did.
type Result struct {
	ReceivedStored  int
	SentStored      int
	Skipped         int
	BackfillPending bool
	Errors          []error
}

// Total is the number of messages newly stored.
func (r Result) Total() int { return r.ReceivedStored + r.SentStored }

// Trigger asks the running poller to sync now. It never blocks: a request
// already queued is enough.
func (s *Syncer) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Run polls until the context is cancelled. It syncs once immediately, then on
// every interval tick and on every Trigger.
//
// A rate-limited or failing account backs off rather than hammering the API;
// the backoff resets as soon as a pass succeeds.
func (s *Syncer) Run(ctx context.Context) error {
	log := s.opts.Logger.With("account", s.store.Name())

	backoff := time.Duration(0)
	for {
		res, err := s.Sync(ctx)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return err
		case err != nil:
			backoff = nextBackoff(backoff, s.opts.Interval)
			log.Warn("sync failed", "error", err, "retry_in", backoff)
		default:
			backoff = 0
			if res.Total() > 0 {
				log.Info("sync stored messages",
					"received", res.ReceivedStored, "sent", res.SentStored,
					"backfill_pending", res.BackfillPending)
			}
		}

		wait := s.opts.Interval
		if backoff > 0 {
			wait = backoff
		}
		// A backfill still has pages to fetch: come back promptly rather than
		// making the user wait a full interval per page.
		if err == nil && res.BackfillPending {
			wait = time.Second
		}
		if wait <= 0 {
			// Polling is disabled: only explicit triggers and webhooks sync.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-s.trigger:
				continue
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-s.trigger:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func nextBackoff(cur, interval time.Duration) time.Duration {
	const maxBackoff = 15 * time.Minute
	if cur == 0 {
		if interval > 0 {
			return interval
		}
		return 30 * time.Second
	}
	if next := cur * 2; next < maxBackoff {
		return next
	}
	return maxBackoff
}

// Sync runs one pass over both directions. A failure in one direction does not
// stop the other: a rate limit while reading sent mail must not block new
// inbound mail from arriving.
func (s *Syncer) Sync(ctx context.Context) (Result, error) {
	var res Result

	rErr := s.syncDirection(ctx, store.KindReceived, &res)
	sErr := s.syncDirection(ctx, store.KindSent, &res)

	err := errors.Join(rErr, sErr)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	return res, err
}

// direction abstracts the two listings, which differ only in endpoints and
// destination mailbox.
type direction struct {
	kind    string
	mailbox string
	list    func(context.Context, resend.ListParams) (*resend.Page, error)
	get     func(context.Context, string) (*resend.Email, error)
}

func (s *Syncer) direction(kind string) direction {
	if kind == store.KindSent {
		return direction{
			kind: store.KindSent, mailbox: store.Sent,
			list: s.api.ListSent, get: s.api.GetSent,
		}
	}
	return direction{
		kind: store.KindReceived, mailbox: store.Inbox,
		list: s.api.ListReceived, get: s.api.GetReceived,
	}
}

func (s *Syncer) syncDirection(ctx context.Context, kind string, res *Result) error {
	dir := s.direction(kind)
	log := s.opts.Logger.With("account", s.store.Name(), "kind", kind)

	st, err := s.store.SyncState(ctx, kind)
	if err != nil {
		return err
	}
	mbox, err := s.store.Mailbox(ctx, dir.mailbox)
	if err != nil {
		return err
	}

	stored, err := s.incremental(ctx, dir, mbox.ID, &st, res)
	if err != nil {
		st.LastError = err.Error()
		_ = s.store.SaveSyncState(ctx, st)
		return fmt.Errorf("mailsync: %s incremental: %w", kind, err)
	}

	// The backfill only runs once the incremental pass is clean, so new mail
	// is never delayed behind a long history walk.
	if !st.BackfillDone {
		n, err := s.backfillPage(ctx, dir, mbox.ID, &st, res)
		if err != nil {
			st.LastError = err.Error()
			_ = s.store.SaveSyncState(ctx, st)
			return fmt.Errorf("mailsync: %s backfill: %w", kind, err)
		}
		stored += n
		res.BackfillPending = res.BackfillPending || !st.BackfillDone
	}

	st.LastSyncAt = s.opts.Clock()
	st.LastError = ""
	if err := s.store.SaveSyncState(ctx, st); err != nil {
		return err
	}
	if stored > 0 {
		log.Debug("stored messages", "count", stored, "mailbox", dir.mailbox)
		if s.opts.Notifier != nil {
			s.opts.Notifier.MailboxChanged(s.store.Name(), mbox.ID)
		}
	}
	return nil
}

// incremental walks pages from the newest row until it reaches the watermark,
// then stores what it found oldest-first so UIDs follow arrival order.
func (s *Syncer) incremental(ctx context.Context, dir direction, mailboxID int64, st *store.SyncState, res *Result) (int, error) {
	var (
		fresh  []resend.Summary
		after  string
		newest string
	)

	// Guard against a pathological first run with no watermark: the backfill
	// pass owns the deep history, so stop after a bounded number of pages.
	const maxPages = 50

	for page := 0; page < maxPages; page++ {
		p, err := dir.list(ctx, resend.ListParams{After: after, Limit: s.opts.PageSize})
		if err != nil {
			return 0, err
		}
		if len(p.Items) == 0 {
			if !st.BackfillDone && st.BackfillCursor == "" {
				// Nothing at all: there is no history to backfill either.
				st.BackfillDone = true
			}
			break
		}
		if newest == "" {
			newest = p.Items[0].ID
		}

		reachedKnown := false
		for _, item := range p.Items {
			if st.NewestID != "" && item.ID == st.NewestID {
				reachedKnown = true
				break
			}
			fresh = append(fresh, item)
		}
		if reachedKnown || !p.HasMore {
			if !p.HasMore && st.BackfillCursor == "" {
				// The whole history fits in what we just walked.
				st.BackfillDone = true
			}
			break
		}
		if st.NewestID == "" {
			// First run: take one page here and let the backfill do the rest,
			// so the user sees recent mail in their client almost immediately.
			break
		}
		after = p.Items[len(p.Items)-1].ID
	}

	n, oldestFailure := s.storeBatch(ctx, dir, mailboxID, fresh, res)

	// The watermark may only move past messages that were actually handled.
	// Advancing it over a message that failed to fetch would lose that message
	// for good, since nothing else ever looks above the watermark again. Held
	// back, the next poll simply retries it: everything newer is already
	// stored and is skipped by the dedupe check without refetching a body.
	switch {
	case oldestFailure < 0:
		if newest != "" {
			st.NewestID = newest
		}
	case oldestFailure < len(fresh)-1:
		st.NewestID = fresh[oldestFailure+1].ID
	default:
		// Even the oldest message of the batch failed: keep the watermark.
	}

	if st.BackfillCursor == "" && len(fresh) > 0 {
		st.BackfillCursor = fresh[len(fresh)-1].ID
	}
	return n, nil
}

// backfillPage fetches one page of older history per call, so a long backfill
// interleaves with incremental passes instead of blocking them.
func (s *Syncer) backfillPage(ctx context.Context, dir direction, mailboxID int64, st *store.SyncState, res *Result) (int, error) {
	p, err := dir.list(ctx, resend.ListParams{After: st.BackfillCursor, Limit: s.opts.PageSize})
	if err != nil {
		return 0, err
	}
	if len(p.Items) == 0 {
		st.BackfillDone = true
		return 0, nil
	}

	n, oldestFailure := s.storeBatch(ctx, dir, mailboxID, p.Items, res)

	// Unlike the incremental watermark, the backfill cursor always advances.
	// It walks a fixed history exactly once, so holding it back on a message
	// that cannot be fetched would spin on that message instead of finishing.
	// The failure is reported in Result and logged, and `ferry sync --backfill`
	// resets the cursor for another full walk.
	st.BackfillCursor = p.Items[len(p.Items)-1].ID
	if !p.HasMore && oldestFailure < 0 {
		st.BackfillDone = true
	}
	return n, nil
}

// storeBatch stores a newest-first batch in oldest-first order, so that UIDs
// increase with arrival time the way a mail client expects.
//
// It returns how many messages it stored and the index of the oldest one that
// failed, or -1 if none did. Because the batch is processed oldest first, every
// item at a higher index than that failure was handled, which is what lets the
// caller advance its cursor exactly as far as is safe.
func (s *Syncer) storeBatch(ctx context.Context, dir direction, mailboxID int64, items []resend.Summary, res *Result) (stored, oldestFailure int) {
	oldestFailure = -1
	for i := len(items) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			if oldestFailure < 0 {
				oldestFailure = i
			}
			return stored, oldestFailure
		}
		ok, err := s.storeOne(ctx, dir, mailboxID, items[i])
		switch {
		case err != nil:
			if oldestFailure < 0 {
				oldestFailure = i
			}
			res.Errors = append(res.Errors, err)
			s.opts.Logger.Warn("skipping message",
				"account", s.store.Name(), "kind", dir.kind, "id", items[i].ID, "error", err)
		case ok:
			stored++
			if dir.kind == store.KindSent {
				res.SentStored++
			} else {
				res.ReceivedStored++
			}
		default:
			res.Skipped++
		}
	}
	return stored, oldestFailure
}

// storeOne fetches and files a single message. It reports false when the
// message was deliberately not stored: already present, or deleted locally.
func (s *Syncer) storeOne(ctx context.Context, dir direction, mailboxID int64, sum resend.Summary) (bool, error) {
	// A message the user deleted must never come back.
	tombstoned, err := s.store.Tombstoned(ctx, sum.ID)
	if err != nil {
		return false, err
	}
	if tombstoned {
		return false, nil
	}
	if existing, err := s.store.FindByResendID(ctx, sum.ID); err == nil && existing != nil {
		return false, nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}

	email, err := dir.get(ctx, sum.ID)
	if err != nil {
		if resend.IsNotFound(err) {
			// Gone from Resend between listing and fetching; nothing to store.
			return false, nil
		}
		return false, fmt.Errorf("fetch %s: %w", sum.ID, err)
	}

	raw, err := s.materialise(ctx, dir, email)
	if err != nil {
		return false, err
	}
	if int64(len(raw)) > s.opts.MaxMessageBytes {
		return false, fmt.Errorf("message %s is %d bytes, over the %d byte limit",
			sum.ID, len(raw), s.opts.MaxMessageBytes)
	}

	idx := mailmime.Parse(raw)
	messageID := firstNonEmpty(idx.MessageID, email.MessageID)

	// A sent message Ferry submitted is already in Sent, filed there by the
	// SMTP server, and Mail may also have APPENDed its own copy. Match it by
	// Message-ID and attach the Resend id instead of storing a duplicate.
	if messageID != "" {
		if existing, err := s.store.FindByMessageID(ctx, messageID); err == nil && existing != nil {
			if existing.ResendID == "" {
				if err := s.store.LinkResendID(ctx, existing.ID, sum.ID); err != nil {
					return false, err
				}
			}
			return false, nil
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
	}

	internalDate := email.CreatedAt
	if internalDate.IsZero() {
		internalDate = sum.CreatedAt
	}
	if internalDate.IsZero() {
		internalDate = s.opts.Clock()
	}

	flags := []string{}
	if dir.kind == store.KindSent {
		// Mail shows the user's own sent messages as read; Resend has no read
		// state to tell us otherwise.
		flags = append(flags, `\Seen`)
	}

	_, err = s.store.Append(ctx, mailboxID, &store.NewMessage{
		Raw:          raw,
		ResendID:     sum.ID,
		MessageID:    messageID,
		InternalDate: internalDate,
		SentDate:     idx.Date,
		Subject:      idx.Subject,
		From:         firstNonEmpty(idx.From, email.From),
		To:           firstNonEmpty(idx.To, strings.Join(email.To, ", ")),
		SearchText:   idx.Text,
		Flags:        flags,
	})
	if err != nil {
		return false, fmt.Errorf("store %s: %w", sum.ID, err)
	}
	return true, nil
}

// materialise produces the full RFC 5322 message. Resend's raw download is
// preferred because it is the message as it actually arrived, signatures and
// all; when it is absent or its signed URL has expired, the message is rebuilt
// from the structured fields and the attachments are fetched individually.
func (s *Syncer) materialise(ctx context.Context, dir direction, email *resend.Email) ([]byte, error) {
	if email.Raw != nil && email.Raw.DownloadURL != "" {
		raw, err := s.download(ctx, email.Raw.DownloadURL)
		switch {
		case err == nil && len(raw) > 0:
			return raw, nil
		case err != nil && !errors.Is(err, resend.ErrExpired):
			return nil, fmt.Errorf("download raw message: %w", err)
		}
		s.opts.Logger.Debug("raw download unavailable, synthesising MIME",
			"account", s.store.Name(), "id", email.ID)
	}

	attachments, err := s.fetchAttachments(ctx, dir, email)
	if err != nil {
		return nil, err
	}

	raw, err := mailmime.Build(&mailmime.Message{
		MessageID:   email.MessageID,
		From:        email.From,
		To:          email.To,
		Cc:          email.Cc,
		Subject:     email.Subject,
		Date:        email.CreatedAt,
		Text:        email.Text,
		HTML:        email.HTML,
		Headers:     email.Headers,
		Attachments: attachments,
	})
	if err != nil {
		return nil, fmt.Errorf("synthesise MIME for %s: %w", email.ID, err)
	}
	return raw, nil
}

// fetchAttachments downloads each attachment body. Only received mail exposes
// per-attachment download URLs; for sent mail the metadata is recorded as a
// placeholder part so the user can see what was attached.
func (s *Syncer) fetchAttachments(ctx context.Context, dir direction, email *resend.Email) ([]mailmime.Attachment, error) {
	if len(email.Attachments) == 0 {
		return nil, nil
	}
	out := make([]mailmime.Attachment, 0, len(email.Attachments))
	for _, a := range email.Attachments {
		att := mailmime.Attachment{
			Filename:    a.Filename,
			ContentType: a.ContentType,
			ContentID:   a.ContentID,
		}
		if dir.kind != store.KindReceived {
			out = append(out, placeholder(att, "This attachment was sent through Resend and its contents are not retrievable."))
			continue
		}

		info, err := s.api.GetReceivedAttachment(ctx, email.ID, a.ID)
		if err != nil {
			if resend.IsNotFound(err) || errors.Is(err, resend.ErrExpired) {
				out = append(out, placeholder(att, "This attachment was no longer available when Ferry synced the message."))
				continue
			}
			return nil, fmt.Errorf("attachment %s of %s: %w", a.ID, email.ID, err)
		}
		data, err := s.download(ctx, info.DownloadURL)
		if err != nil {
			if errors.Is(err, resend.ErrExpired) {
				out = append(out, placeholder(att, "This attachment's download link had expired when Ferry synced the message."))
				continue
			}
			return nil, fmt.Errorf("download attachment %s of %s: %w", a.ID, email.ID, err)
		}
		att.Data = data
		out = append(out, att)
	}
	return out, nil
}

// placeholder replaces an unavailable attachment with a note, so the message
// still records that something was attached instead of quietly losing it.
func placeholder(a mailmime.Attachment, note string) mailmime.Attachment {
	name := a.Filename
	if name == "" {
		name = "attachment"
	}
	return mailmime.Attachment{
		Filename:    name + ".txt",
		ContentType: "text/plain; charset=utf-8",
		Data:        []byte(note + "\n\nOriginal file name: " + name + "\n"),
	}
}

func (s *Syncer) download(ctx context.Context, url string) ([]byte, error) {
	rc, err := s.api.Download(ctx, url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, s.opts.MaxMessageBytes+1))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ResetBackfill clears both cursors for an account so that the next sync walks
// the whole history again. Messages already stored are skipped by the dedupe
// check, and messages the user deleted stay deleted, so a re-walk is safe: it
// costs API calls, not duplicates. It is how `ferry sync --backfill` recovers
// history that a failed page skipped.
func (s *Syncer) ResetBackfill(ctx context.Context) error {
	for _, kind := range []string{store.KindReceived, store.KindSent} {
		st, err := s.store.SyncState(ctx, kind)
		if err != nil {
			return err
		}
		st.NewestID = ""
		st.BackfillCursor = ""
		st.BackfillDone = false
		st.LastError = ""
		if err := s.store.SaveSyncState(ctx, st); err != nil {
			return err
		}
	}
	return nil
}

// Account returns the account this syncer serves.
func (s *Syncer) Account() string { return s.store.Name() }
