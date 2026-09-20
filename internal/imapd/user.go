package imapd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/LucasStbnr/ferry/internal/store"
)

// user is one account's shared mailbox set. Every session for that account
// works through the same user, so an update from one connection or from the
// sync engine reaches the others.
type user struct {
	store *store.AccountStore

	mu        sync.Mutex
	mailboxes map[string]*mailbox
}

func newUser(as *store.AccountStore) *user {
	return &user{store: as, mailboxes: map[string]*mailbox{}}
}

// load reads the mailbox list and the contents of each mailbox from the store.
func (u *user) load(ctx context.Context) error {
	recs, err := u.store.Mailboxes(ctx)
	if err != nil {
		return err
	}

	u.mu.Lock()
	seen := make(map[string]bool, len(recs))
	var fresh []*mailbox
	for _, rec := range recs {
		seen[rec.Name] = true
		if mbox, ok := u.mailboxes[rec.Name]; ok {
			fresh = append(fresh, mbox)
			continue
		}
		mbox := newMailbox(u, rec)
		u.mailboxes[rec.Name] = mbox
		fresh = append(fresh, mbox)
	}
	for name := range u.mailboxes {
		if !seen[name] {
			delete(u.mailboxes, name)
		}
	}
	u.mu.Unlock()

	for _, mbox := range fresh {
		if err := mbox.reload(ctx); err != nil {
			return err
		}
	}
	return nil
}

// refresh reloads one mailbox by store id, which is how the sync engine wakes
// idling clients after filing new mail.
func (u *user) refresh(ctx context.Context, mailboxID int64) error {
	u.mu.Lock()
	var target *mailbox
	for _, mbox := range u.mailboxes {
		if mbox.id == mailboxID {
			target = mbox
			break
		}
	}
	u.mu.Unlock()
	if target == nil {
		// A mailbox we have not opened yet: pick it up with the whole list.
		return u.load(ctx)
	}
	return target.reload(ctx)
}

func (u *user) mailbox(name string) (*mailbox, error) {
	name = canonical(name)
	u.mu.Lock()
	defer u.mu.Unlock()
	mbox, ok := u.mailboxes[name]
	if !ok {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}
	return mbox, nil
}

func (u *user) list() []*mailbox {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]*mailbox, 0, len(u.mailboxes))
	for _, mbox := range u.mailboxes {
		out = append(out, mbox)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// canonical normalises a mailbox name as it arrives from a client.
//
// It folds INBOX, which RFC 3501 requires to be case-insensitive, and it
// strips a leading hierarchy delimiter. The second part is not in any RFC: a
// client configured with a path prefix of "/" (or with an empty prefix, which
// Apple Mail treats the same way) asks for "/Trash" rather than "Trash".
// Rejecting those is technically correct and practically useless, because the
// visible symptom is that deleting a message does nothing at all.
func canonical(name string) string {
	name = strings.TrimLeft(name, string(store.Delim))
	if strings.EqualFold(name, store.Inbox) {
		return store.Inbox
	}
	return name
}

// imapError maps a store error to the right IMAP tagged response. Returning a
// bare Go error would give the client a generic failure and, in Mail's case, a
// dialog that says nothing useful.
func imapError(err error) error {
	if err == nil {
		return nil
	}
	var ie *imap.Error
	if errors.As(err, &ie) {
		return err
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	case errors.Is(err, store.ErrAlreadyExists):
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeAlreadyExists,
			Text: "Mailbox already exists",
		}
	case errors.Is(err, context.Canceled):
		return err
	}
	return &imap.Error{Type: imap.StatusResponseTypeNo, Text: err.Error()}
}

func (u *user) listMailboxes(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	if len(patterns) == 0 {
		// A bare LIST "" "" asks for the hierarchy delimiter.
		return w.WriteList(&imap.ListData{
			Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect},
			Delim: store.Delim,
		})
	}

	var out []imap.ListData
	for _, mbox := range u.list() {
		matched := false
		for _, pattern := range patterns {
			if imapserver.MatchList(mbox.name, store.Delim, ref, pattern) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}

		mbox.mu.Lock()
		subscribed := mbox.subscribed
		data := imap.ListData{Mailbox: mbox.name, Delim: store.Delim, Attrs: mbox.attrs()}
		if options.ReturnStatus != nil {
			data.Status = mbox.statusDataLocked(options.ReturnStatus)
		}
		mbox.mu.Unlock()

		if options.SelectSubscribed && !subscribed {
			continue
		}
		if options.SelectSpecialUse && len(data.Attrs) == 0 {
			continue
		}
		if subscribed {
			data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
		}
		out = append(out, data)
	}

	sort.Slice(out, func(i, j int) bool {
		// INBOX first: Mail expects to find it at the top of the list.
		if (out[i].Mailbox == store.Inbox) != (out[j].Mailbox == store.Inbox) {
			return out[i].Mailbox == store.Inbox
		}
		return out[i].Mailbox < out[j].Mailbox
	})

	for i := range out {
		if err := w.WriteList(&out[i]); err != nil {
			return err
		}
	}
	return nil
}

func (u *user) namespace() *imap.NamespaceData {
	// One personal namespace with no prefix. Ferry has no shared or other-user
	// mailboxes, and saying so keeps Mail from probing for them.
	return &imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{{Delim: store.Delim}},
	}
}

func (u *user) String() string { return fmt.Sprintf("imapd.user(%s)", u.store.Name()) }
