package imapd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/LucasStbnr/ferry/internal/mailmime"
	"github.com/LucasStbnr/ferry/internal/store"
)

// session is one IMAP connection. It is bound to a single account from the
// moment it authenticates and holds no reference to any other, which is how
// two Resend accounts stay separate accounts in the client.
type session struct {
	server *Server
	log    *slog.Logger

	// ctx is the connection's context; it is cancelled when the client goes
	// away, so a long FETCH does not outlive the socket.
	ctx    context.Context
	cancel context.CancelFunc

	user     *user
	selected *selectedMailbox
}

// selectedMailbox is the per-connection view of a selected mailbox: the shared
// mailbox plus this connection's own update queue and SEARCHRES.
type selectedMailbox struct {
	*mailbox
	tracker   *imapserver.SessionTracker
	searchRes imap.UIDSet
	readOnly  bool
}

var (
	_ imapserver.Session            = (*session)(nil)
	_ imapserver.SessionNamespace   = (*session)(nil)
	_ imapserver.SessionMove        = (*session)(nil)
	_ imapserver.SessionAppendLimit = (*session)(nil)
)

// Close releases the session. It is called when the connection ends.
func (s *session) Close() error {
	s.unselect()
	s.cancel()
	return nil
}

func (s *session) unselect() {
	if s.selected != nil {
		s.selected.tracker.Close()
		s.selected = nil
	}
}

// Login authenticates the connection against one account's app password.
func (s *session) Login(username, password string) error {
	u, err := s.server.authenticate(s.ctx, username, password)
	if err != nil {
		s.log.Warn("imap login failed", "username", username, "error", err)
		// Never distinguish an unknown account from a wrong password: that
		// would tell an attacker which account names exist.
		return imapserver.ErrAuthFailed
	}
	s.user = u
	s.log = s.log.With("account", u.store.Name())
	s.log.Info("imap login")
	return nil
}

func (s *session) requireAuth() error {
	if s.user == nil {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Not authenticated"}
	}
	return nil
}

func (s *session) requireSelected() (*selectedMailbox, error) {
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	if s.selected == nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeBad, Text: "No mailbox selected"}
	}
	return s.selected, nil
}

// Namespace implements the NAMESPACE command.
func (s *session) Namespace() (*imap.NamespaceData, error) {
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	return s.user.namespace(), nil
}

// AppendLimit caps APPEND, so an oversized draft is refused with a clear IMAP
// error instead of filling the disk.
func (s *session) AppendLimit() uint32 { return uint32(s.server.appendLimit()) }

// Select opens a mailbox.
func (s *session) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	mbox, err := s.user.mailbox(name)
	if err != nil {
		return nil, err
	}
	// Re-read: another connection or the sync engine may have changed it.
	if err := mbox.reload(s.ctx); err != nil {
		return nil, imapError(err)
	}

	s.unselect()
	s.selected = &selectedMailbox{
		mailbox:  mbox,
		tracker:  mbox.tracker.NewSession(),
		readOnly: options != nil && options.ReadOnly,
	}

	mbox.mu.Lock()
	defer mbox.mu.Unlock()
	return mbox.selectDataLocked(), nil
}

// Unselect closes the selected mailbox without expunging.
func (s *session) Unselect() error {
	s.unselect()
	return nil
}

// Create adds a mailbox.
func (s *session) Create(name string, options *imap.CreateOptions) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	specialUse := ""
	if options != nil && len(options.SpecialUse) > 0 {
		specialUse = string(options.SpecialUse[0])
	}
	name = strings.TrimRight(canonical(name), string(store.Delim))
	if err := s.user.store.CreateMailbox(s.ctx, name, specialUse); err != nil {
		return imapError(err)
	}
	return imapError(s.user.load(s.ctx))
}

// Delete removes a mailbox and tombstones what was in it.
func (s *session) Delete(name string) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	if err := s.user.store.DeleteMailbox(s.ctx, canonical(name)); err != nil {
		return imapError(err)
	}
	return imapError(s.user.load(s.ctx))
}

// Rename moves a mailbox and its descendants.
func (s *session) Rename(name, newName string, options *imap.RenameOptions) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	newName = strings.TrimRight(canonical(newName), string(store.Delim))
	if err := s.user.store.RenameMailbox(s.ctx, canonical(name), newName); err != nil {
		return imapError(err)
	}
	return imapError(s.user.load(s.ctx))
}

// Subscribe records a subscription.
func (s *session) Subscribe(name string) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	if err := s.user.store.SetSubscribed(s.ctx, canonical(name), true); err != nil {
		return imapError(err)
	}
	return imapError(s.user.load(s.ctx))
}

// Unsubscribe removes a subscription.
func (s *session) Unsubscribe(name string) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	if err := s.user.store.SetSubscribed(s.ctx, canonical(name), false); err != nil {
		return imapError(err)
	}
	return imapError(s.user.load(s.ctx))
}

// List implements LIST and LSUB.
func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	if err := s.user.load(s.ctx); err != nil {
		return imapError(err)
	}
	return s.user.listMailboxes(w, ref, patterns, options)
}

// Status answers STATUS for a mailbox that need not be selected.
func (s *session) Status(name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	mbox, err := s.user.mailbox(name)
	if err != nil {
		return nil, err
	}
	if err := mbox.reload(s.ctx); err != nil {
		return nil, imapError(err)
	}
	return mbox.statusData(options), nil
}

// Append stores a message the client uploaded: a draft Mail is saving, or its
// own copy of a message it just sent.
func (s *session) Append(name string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	mbox, err := s.user.mailbox(name)
	if err != nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	}

	limit := s.server.appendLimit()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, limit+1))
	if err != nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Could not read message: " + err.Error()}
	}
	if n > limit {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: fmt.Sprintf("Message is larger than %d bytes", limit),
		}
	}
	raw := buf.Bytes()

	idx := mailmime.Parse(raw)
	internalDate := time.Now()
	if options != nil && !options.Time.IsZero() {
		internalDate = options.Time
	}
	var flags []string
	if options != nil {
		for _, f := range options.Flags {
			flags = append(flags, string(f))
		}
	}

	uid, err := s.user.store.Append(s.ctx, mbox.id, &store.NewMessage{
		Raw:          raw,
		MessageID:    idx.MessageID,
		InternalDate: internalDate,
		SentDate:     idx.Date,
		Subject:      idx.Subject,
		From:         idx.From,
		To:           idx.To,
		SearchText:   idx.Text,
		Flags:        flags,
	})
	if err != nil {
		return nil, imapError(err)
	}
	if err := mbox.reload(s.ctx); err != nil {
		return nil, imapError(err)
	}

	mbox.mu.Lock()
	uidValidity := mbox.uidValidity
	mbox.mu.Unlock()

	return &imap.AppendData{UIDValidity: uidValidity, UID: imap.UID(uid)}, nil
}

// Poll delivers queued updates between commands.
func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.selected == nil {
		return nil
	}
	return s.selected.tracker.Poll(w, allowExpunge)
}

// Idle holds the connection open and streams updates until the client stops it.
func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if s.selected == nil {
		// IDLE in the authenticated state is legal; there is simply nothing to
		// report until a mailbox is selected.
		<-stop
		return nil
	}
	return s.selected.tracker.Idle(w, stop)
}

// Expunge removes messages flagged \Deleted, writing tombstones so that the
// next sync does not bring them back from Resend.
func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	sel, err := s.requireSelected()
	if err != nil {
		return err
	}
	if sel.readOnly {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Mailbox is read-only"}
	}

	sel.mu.Lock()
	var ids []int64
	for _, m := range sel.msgs {
		if uids != nil && !uids.Contains(m.uid) {
			continue
		}
		if _, ok := m.flags[imap.FlagDeleted]; ok {
			ids = append(ids, m.id)
		}
	}
	sel.mu.Unlock()

	if len(ids) == 0 {
		s.log.Debug("expunge matched nothing", "mailbox", sel.name)
		return nil
	}
	if err := s.user.store.Expunge(s.ctx, ids); err != nil {
		return imapError(err)
	}
	s.log.Info("expunged messages", "mailbox", sel.name, "count", len(ids))
	return imapError(sel.reload(s.ctx))
}

// Copy duplicates messages into another mailbox.
func (s *session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	sel, err := s.requireSelected()
	if err != nil {
		return nil, err
	}
	destBox, err := s.user.mailbox(dest)
	if err != nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	}

	ids, srcUIDs := sel.resolve(numSet)
	if len(ids) == 0 {
		return nil, nil
	}
	res, err := s.user.store.Copy(s.ctx, ids, destBox.id)
	if err != nil {
		s.log.Warn("copy failed", "from", sel.name, "to", destBox.name, "count", len(ids), "error", err)
		return nil, imapError(err)
	}
	s.log.Info("copied messages", "from", sel.name, "to", destBox.name, "count", len(res))
	if err := destBox.reload(s.ctx); err != nil {
		return nil, imapError(err)
	}

	destBox.mu.Lock()
	uidValidity := destBox.uidValidity
	destBox.mu.Unlock()

	return &imap.CopyData{
		UIDValidity: uidValidity,
		SourceUIDs:  uidSetOf(srcUIDs),
		DestUIDs:    destUIDSet(res),
	}, nil
}

// Move relocates messages, which is what Mail does when you drag a message to
// another folder or delete it into Trash.
func (s *session) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	sel, err := s.requireSelected()
	if err != nil {
		return err
	}
	if sel.readOnly {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Mailbox is read-only"}
	}
	destBox, err := s.user.mailbox(dest)
	if err != nil {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	}

	ids, srcUIDs := sel.resolve(numSet)
	if len(ids) == 0 {
		return nil
	}

	res, err := s.user.store.Move(s.ctx, ids, destBox.id)
	if err != nil {
		s.log.Warn("move failed", "from", sel.name, "to", destBox.name, "count", len(ids), "error", err)
		return imapError(err)
	}
	s.log.Info("moved messages", "from", sel.name, "to", destBox.name, "count", len(res))

	destBox.mu.Lock()
	uidValidity := destBox.uidValidity
	destBox.mu.Unlock()

	if err := w.WriteCopyData(&imap.CopyData{
		UIDValidity: uidValidity,
		SourceUIDs:  uidSetOf(srcUIDs),
		DestUIDs:    destUIDSet(res),
	}); err != nil {
		return err
	}

	// The EXPUNGEs are deliberately not written here. Every removal from a
	// mailbox goes through the tracker, which is the only thing that knows
	// each session's view of the sequence numbers; writing them inline as
	// well would both double-report them to this connection and leave the
	// tracker believing the messages were still there.
	if err := destBox.reload(s.ctx); err != nil {
		return imapError(err)
	}
	return imapError(sel.reload(s.ctx))
}

// Store applies an IMAP STORE to the flags of a set of messages.
func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	sel, err := s.requireSelected()
	if err != nil {
		return err
	}
	if sel.readOnly {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Mailbox is read-only"}
	}

	ids, _ := sel.resolve(numSet)
	if len(ids) == 0 {
		return nil
	}

	var op store.FlagsOp
	switch flags.Op {
	case imap.StoreFlagsSet:
		op = store.FlagsSet
	case imap.StoreFlagsAdd:
		op = store.FlagsAdd
	case imap.StoreFlagsDel:
		op = store.FlagsDel
	default:
		return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "Unknown STORE operation"}
	}

	strs := make([]string, 0, len(flags.Flags))
	for _, f := range flags.Flags {
		strs = append(strs, string(f))
	}
	if _, err := s.user.store.StoreFlags(s.ctx, ids, op, strs); err != nil {
		return imapError(err)
	}
	s.log.Debug("stored flags", "mailbox", sel.name, "count", len(ids), "op", flags.Op, "flags", strs)
	if err := sel.reload(s.ctx); err != nil {
		return imapError(err)
	}

	if flags.Silent {
		return nil
	}
	return s.Fetch(w, numSet, &imap.FetchOptions{Flags: true})
}
