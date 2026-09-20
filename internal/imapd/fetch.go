package imapd

import (
	"bytes"
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/LucasStbnr/ferry/internal/store"
)

// resolve turns an IMAP number set into store ids and the UIDs they map to,
// in mailbox order. It handles the "*" wildcard and the SEARCHRES "$" marker,
// both of which are relative to the mailbox as this session currently sees it.
func (sel *selectedMailbox) resolve(numSet imap.NumSet) (ids []int64, uids []imap.UID) {
	sel.mu.Lock()
	defer sel.mu.Unlock()

	numSet = sel.staticNumSetLocked(numSet)
	for i, m := range sel.msgs {
		if !sel.containsLocked(numSet, uint32(i)+1, m.uid) {
			continue
		}
		ids = append(ids, m.id)
		uids = append(uids, m.uid)
	}
	return ids, uids
}

func (sel *selectedMailbox) containsLocked(numSet imap.NumSet, seqNum uint32, uid imap.UID) bool {
	switch set := numSet.(type) {
	case imap.SeqSet:
		encoded := sel.tracker.EncodeSeqNum(seqNum)
		return encoded != 0 && set.Contains(encoded)
	case imap.UIDSet:
		return set.Contains(uid)
	}
	return false
}

// staticNumSetLocked replaces the dynamic "*" with the mailbox's current
// maximum and "$" with the last SEARCH result, so a set evaluated later in the
// command still means what the client asked for.
func (sel *selectedMailbox) staticNumSetLocked(numSet imap.NumSet) imap.NumSet {
	if imap.IsSearchRes(numSet) {
		return sel.searchRes
	}
	switch set := numSet.(type) {
	case imap.SeqSet:
		highest := uint32(len(sel.msgs))
		for i := range set {
			staticRange(&set[i].Start, &set[i].Stop, highest)
		}
		return set
	case imap.UIDSet:
		highest := uint32(sel.uidNext) - 1
		for i := range set {
			staticRange((*uint32)(&set[i].Start), (*uint32)(&set[i].Stop), highest)
		}
		return set
	}
	return numSet
}

func staticRange(start, stop *uint32, highest uint32) {
	dynamic := false
	if *start == 0 {
		*start, dynamic = highest, true
	}
	if *stop == 0 {
		*stop, dynamic = highest, true
	}
	if dynamic && *start > *stop {
		*start, *stop = *stop, *start
	}
}

func uidSetOf(uids []imap.UID) imap.UIDSet {
	var set imap.UIDSet
	for _, uid := range uids {
		set.AddNum(uid)
	}
	return set
}

func destUIDSet(res []store.CopyResult) imap.UIDSet {
	var set imap.UIDSet
	for _, r := range res {
		set.AddNum(imap.UID(r.DestUID))
	}
	return set
}

// Fetch answers FETCH and UID FETCH.
//
// A non-peeking BODY[] implicitly marks the message read, which is how Mail's
// preview pane clears the unread badge. That write goes to the store, so the
// state survives a restart, and is announced to every other session.
func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	sel, err := s.requireSelected()
	if err != nil {
		return err
	}

	markSeen := false
	for _, bs := range options.BodySection {
		if !bs.Peek {
			markSeen = true
			break
		}
	}
	if markSeen && sel.readOnly {
		markSeen = false
	}

	sel.mu.Lock()
	staticSet := sel.staticNumSetLocked(numSet)
	type target struct {
		seqNum uint32
		msg    *message
	}
	var targets []target
	for i, m := range sel.msgs {
		if sel.containsLocked(staticSet, uint32(i)+1, m.uid) {
			targets = append(targets, target{uint32(i) + 1, m})
		}
	}
	sel.mu.Unlock()

	var toMarkSeen []int64
	if markSeen {
		for _, t := range targets {
			sel.mu.Lock()
			_, seen := t.msg.flags[imap.FlagSeen]
			sel.mu.Unlock()
			if !seen {
				toMarkSeen = append(toMarkSeen, t.msg.id)
			}
		}
		if len(toMarkSeen) > 0 {
			if _, err := s.user.store.StoreFlags(s.ctx, toMarkSeen, store.FlagsAdd, []string{`\Seen`}); err != nil {
				return imapError(err)
			}
			sel.mu.Lock()
			for _, t := range targets {
				for _, id := range toMarkSeen {
					if t.msg.id == id {
						t.msg.flags[imap.FlagSeen] = struct{}{}
						sel.mailbox.tracker.QueueMessageFlags(t.seqNum, t.msg.uid, t.msg.flagList(), sel.tracker)
					}
				}
			}
			sel.mu.Unlock()
		}
	}

	for _, t := range targets {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if err := s.fetchOne(w, sel, t.seqNum, t.msg, options); err != nil {
			return err
		}
	}
	return nil
}

// needsBody reports whether answering this FETCH requires reading the blob.
// A client polling flags should not cause a disk read per message.
func needsBody(options *imap.FetchOptions) bool {
	return options.Envelope ||
		options.BodyStructure != nil ||
		len(options.BodySection) > 0 ||
		len(options.BinarySection) > 0 ||
		len(options.BinarySectionSize) > 0
}

func (s *session) fetchOne(w *imapserver.FetchWriter, sel *selectedMailbox, seqNum uint32, m *message, options *imap.FetchOptions) error {
	var raw []byte
	if needsBody(options) {
		var err error
		raw, err = s.user.store.ReadBlob(m.blobHash)
		if err != nil {
			// The index says the message exists but the blob is gone. Report
			// it and serve an empty body rather than failing the whole FETCH,
			// which would make the mailbox unreadable in Mail.
			s.log.Error("message body missing", "mailbox", sel.name, "uid", m.uid, "blob", m.blobHash, "error", err)
			raw = missingBodyPlaceholder(m)
		}
	}

	rw := w.CreateMessage(sel.tracker.EncodeSeqNum(seqNum))
	rw.WriteUID(m.uid)

	if options.Flags {
		sel.mu.Lock()
		flags := m.flagList()
		sel.mu.Unlock()
		rw.WriteFlags(flags)
	}
	if options.InternalDate {
		rw.WriteInternalDate(m.internalDate)
	}
	if options.RFC822Size {
		rw.WriteRFC822Size(m.size)
	}
	if options.Envelope {
		rw.WriteEnvelope(imapserver.ExtractEnvelope(headerOf(raw)))
	}
	if options.BodyStructure != nil {
		rw.WriteBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(raw)))
	}

	for _, bs := range options.BodySection {
		buf := imapserver.ExtractBodySection(bytes.NewReader(raw), bs)
		wc := rw.WriteBodySection(bs, int64(len(buf)))
		_, writeErr := wc.Write(buf)
		closeErr := wc.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, bs := range options.BinarySection {
		buf := imapserver.ExtractBinarySection(bytes.NewReader(raw), bs)
		wc := rw.WriteBinarySection(bs, int64(len(buf)))
		_, writeErr := wc.Write(buf)
		closeErr := wc.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, bss := range options.BinarySectionSize {
		rw.WriteBinarySectionSize(bss, imapserver.ExtractBinarySectionSize(bytes.NewReader(raw), bss))
	}

	return rw.Close()
}

// missingBodyPlaceholder keeps a mailbox readable when a blob has gone missing,
// by giving the client a well-formed message that explains what happened.
func missingBodyPlaceholder(m *message) []byte {
	return []byte(fmt.Sprintf(
		"Subject: [Ferry] Message body unavailable\r\n"+
			"From: ferry@localhost\r\n"+
			"Date: %s\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"\r\n"+
			"Ferry has an index entry for this message but its stored copy is missing.\r\n"+
			"Run `ferry doctor` to check the message store.\r\n",
		m.internalDate.Format("Mon, 02 Jan 2006 15:04:05 -0700")))
}
