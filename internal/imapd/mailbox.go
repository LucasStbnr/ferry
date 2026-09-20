// Package imapd exposes each Resend account as an IMAP server that Apple Mail
// can use as an ordinary account.
//
// The design mirrors go-imap's reference in-memory server, but the state of
// record is the SQLite store. A mailbox keeps the ordered UID list in memory so
// sequence numbers, which IMAP defines positionally, stay cheap; message bodies
// are read from blobs on demand.
//
// State is shared per (account, mailbox) across connections, so a message the
// sync engine files or another client flags reaches every idling client.
package imapd

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/LucasStbnr/ferry/internal/store"
)

// message is the in-memory view of a stored message. Only metadata lives here;
// the body is read from the blob when a FETCH asks for it.
type message struct {
	id           int64
	uid          imap.UID
	size         int64
	internalDate time.Time
	blobHash     string
	flags        map[imap.Flag]struct{}
}

func (m *message) flagList() []imap.Flag {
	out := make([]imap.Flag, 0, len(m.flags))
	for f := range m.flags {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// mailbox is the shared state of one mailbox of one account.
type mailbox struct {
	user    *user
	tracker *imapserver.MailboxTracker

	mu          sync.Mutex
	id          int64
	name        string
	specialUse  string
	uidValidity uint32
	uidNext     imap.UID
	subscribed  bool
	msgs        []*message // ordered by UID; index+1 is the sequence number
}

func newMailbox(u *user, rec store.Mailbox) *mailbox {
	return &mailbox{
		user:        u,
		tracker:     imapserver.NewMailboxTracker(0),
		id:          rec.ID,
		name:        rec.Name,
		specialUse:  rec.SpecialUse,
		uidValidity: rec.UIDValidity,
		uidNext:     imap.UID(rec.UIDNext),
		subscribed:  rec.Subscribed,
	}
}

// reload re-reads the mailbox from the store and tells the tracker what
// changed, which is what turns a background sync into an unsolicited EXISTS on
// an idling client.
func (mbox *mailbox) reload(ctx context.Context) error {
	rows, err := mbox.user.store.Messages(ctx, mbox.id)
	if err != nil {
		return err
	}
	rec, err := mbox.user.store.Mailbox(ctx, mbox.name)
	if err != nil {
		return err
	}

	mbox.mu.Lock()
	defer mbox.mu.Unlock()

	next := make([]*message, 0, len(rows))
	byUID := make(map[imap.UID]*message, len(mbox.msgs))
	for _, m := range mbox.msgs {
		byUID[m.uid] = m
	}

	for _, r := range rows {
		m := &message{
			id:           r.ID,
			uid:          imap.UID(r.UID),
			size:         r.Size,
			internalDate: r.InternalDate,
			blobHash:     r.BlobHash,
			flags:        make(map[imap.Flag]struct{}, len(r.Flags)),
		}
		for _, f := range r.Flags {
			m.flags[imap.Flag(f)] = struct{}{}
		}
		next = append(next, m)
	}

	// Expunges must be reported from the highest sequence number down so the
	// numbers the client still holds stay valid as it applies them.
	present := make(map[imap.UID]bool, len(next))
	for _, m := range next {
		present[m.uid] = true
	}
	expunged := 0
	for i := len(mbox.msgs) - 1; i >= 0; i-- {
		if !present[mbox.msgs[i].uid] {
			mbox.tracker.QueueExpunge(uint32(i) + 1)
			expunged++
		}
	}

	// The tracker treats a count of zero as "no update" and refuses to be
	// decreased, because EXISTS only ever grows and shrinking is expressed by
	// the EXPUNGEs just queued. So announce a count only when it really did
	// grow beyond what those EXPUNGEs already left the tracker believing.
	trackerCount := len(mbox.msgs) - expunged
	if len(next) > trackerCount {
		mbox.tracker.QueueNumMessages(uint32(len(next)))
	}

	mbox.msgs = next
	mbox.uidNext = imap.UID(rec.UIDNext)
	mbox.subscribed = rec.Subscribed

	// Flags that changed underneath us (another client, or the sync engine)
	// are announced so every session converges.
	for i, m := range next {
		if old, ok := byUID[m.uid]; ok && !sameFlags(old.flags, m.flags) {
			mbox.tracker.QueueMessageFlags(uint32(i)+1, m.uid, m.flagList(), nil)
		}
	}
	return nil
}

func sameFlags(a, b map[imap.Flag]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for f := range a {
		if _, ok := b[f]; !ok {
			return false
		}
	}
	return true
}

func (mbox *mailbox) attrs() []imap.MailboxAttr {
	var attrs []imap.MailboxAttr
	if mbox.specialUse != "" {
		attrs = append(attrs, imap.MailboxAttr(mbox.specialUse))
	}
	return attrs
}

func (mbox *mailbox) statusData(options *imap.StatusOptions) *imap.StatusData {
	mbox.mu.Lock()
	defer mbox.mu.Unlock()
	return mbox.statusDataLocked(options)
}

func (mbox *mailbox) statusDataLocked(options *imap.StatusOptions) *imap.StatusData {
	data := imap.StatusData{Mailbox: mbox.name}
	if options.NumMessages {
		n := uint32(len(mbox.msgs))
		data.NumMessages = &n
	}
	if options.UIDNext {
		data.UIDNext = mbox.uidNext
	}
	if options.UIDValidity {
		data.UIDValidity = mbox.uidValidity
	}
	if options.NumUnseen {
		n := mbox.countWithoutFlagLocked(imap.FlagSeen)
		data.NumUnseen = &n
	}
	if options.NumDeleted {
		n := mbox.countWithFlagLocked(imap.FlagDeleted)
		data.NumDeleted = &n
	}
	if options.Size {
		var size int64
		for _, m := range mbox.msgs {
			size += m.size
		}
		data.Size = &size
	}
	if options.NumRecent {
		// Ferry never sets \Recent: it has no meaning for a store several
		// clients and a background sync all touch.
		var n uint32
		data.NumRecent = &n
	}
	return &data
}

func (mbox *mailbox) countWithFlagLocked(flag imap.Flag) uint32 {
	var n uint32
	for _, m := range mbox.msgs {
		if _, ok := m.flags[flag]; ok {
			n++
		}
	}
	return n
}

func (mbox *mailbox) countWithoutFlagLocked(flag imap.Flag) uint32 {
	return uint32(len(mbox.msgs)) - mbox.countWithFlagLocked(flag)
}

func (mbox *mailbox) selectDataLocked() *imap.SelectData {
	flags := mbox.flagsLocked()
	permanent := append(append([]imap.Flag(nil), flags...), imap.FlagWildcard)
	return &imap.SelectData{
		Flags:             flags,
		PermanentFlags:    permanent,
		NumMessages:       uint32(len(mbox.msgs)),
		FirstUnseenSeqNum: mbox.firstUnseenLocked(),
		UIDNext:           mbox.uidNext,
		UIDValidity:       mbox.uidValidity,
	}
}

// flagsLocked lists the flags in use, plus the system flags Mail expects to be
// able to set even on an empty mailbox.
func (mbox *mailbox) flagsLocked() []imap.Flag {
	set := map[imap.Flag]struct{}{
		imap.FlagSeen:     {},
		imap.FlagAnswered: {},
		imap.FlagFlagged:  {},
		imap.FlagDeleted:  {},
		imap.FlagDraft:    {},
	}
	for _, m := range mbox.msgs {
		for f := range m.flags {
			set[f] = struct{}{}
		}
	}
	out := make([]imap.Flag, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (mbox *mailbox) firstUnseenLocked() uint32 {
	for i, m := range mbox.msgs {
		if _, ok := m.flags[imap.FlagSeen]; !ok {
			return uint32(i) + 1
		}
	}
	return 0
}
