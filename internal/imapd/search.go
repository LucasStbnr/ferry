package imapd

import (
	"bufio"
	"bytes"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

// Search answers SEARCH and UID SEARCH.
//
// Criteria that the database can answer (flags, dates, size, and free text)
// are answered from the index. Only header criteria, which IMAP allows against
// any header, fall back to reading the message; they are rare and are applied
// last, after the cheap criteria have already narrowed the candidates.
func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	sel, err := s.requireSelected()
	if err != nil {
		return nil, err
	}

	// Free-text criteria go through FTS5, which is the whole reason Mail's
	// server-side search is usable on a large mailbox.
	textMatches, err := s.textMatches(sel, criteria)
	if err != nil {
		return nil, imapError(err)
	}

	sel.mu.Lock()
	sel.staticSearchCriteriaLocked(criteria)
	type candidate struct {
		seqNum uint32
		msg    *message
	}
	candidates := make([]candidate, 0, len(sel.msgs))
	for i, m := range sel.msgs {
		candidates = append(candidates, candidate{uint32(i) + 1, m})
	}
	snapshot := make(map[*message][]imap.Flag, len(sel.msgs))
	for _, m := range sel.msgs {
		snapshot[m] = m.flagList()
	}
	sel.mu.Unlock()

	var (
		data   imap.SearchData
		seqSet imap.SeqSet
		uidSet imap.UIDSet
	)
	for _, c := range candidates {
		seqNum := sel.tracker.EncodeSeqNum(c.seqNum)
		if !s.matches(c.msg, seqNum, criteria, textMatches, snapshot) {
			continue
		}
		// The UID set is always built: SEARCHRES saves UIDs regardless of the
		// form the client asked its results in.
		uidSet.AddNum(c.msg.uid)

		var num uint32
		switch kind {
		case imapserver.NumKindSeq:
			if seqNum == 0 {
				continue
			}
			seqSet.AddNum(seqNum)
			num = seqNum
		case imapserver.NumKindUID:
			num = uint32(c.msg.uid)
		}
		if data.Min == 0 || num < data.Min {
			data.Min = num
		}
		if num > data.Max {
			data.Max = num
		}
		data.Count++
	}

	switch kind {
	case imapserver.NumKindSeq:
		data.All = seqSet
	case imapserver.NumKindUID:
		data.All = uidSet
	}
	if options != nil && options.ReturnSave {
		sel.mu.Lock()
		sel.searchRes = uidSet
		sel.mu.Unlock()
	}
	return &data, nil
}

// textSet is the result of the indexed part of a search: nil means "no text
// criteria were given", so every message is still a candidate.
type textSet map[int64]bool

// textMatches runs the TEXT, BODY and SUBJECT criteria through the full-text
// index and returns the intersection of their hits.
func (s *session) textMatches(sel *selectedMailbox, criteria *imap.SearchCriteria) (textSet, error) {
	var result textSet

	intersect := func(ids []int64) {
		next := make(textSet, len(ids))
		for _, id := range ids {
			if result == nil || result[id] {
				next[id] = true
			}
		}
		result = next
	}

	for _, text := range criteria.Text {
		ids, err := s.user.store.Search(s.ctx, sel.id, text)
		if err != nil {
			return nil, err
		}
		intersect(ids)
	}
	for _, body := range criteria.Body {
		ids, err := s.user.store.SearchColumn(s.ctx, sel.id, "body", body)
		if err != nil {
			return nil, err
		}
		intersect(ids)
	}
	// SUBJECT arrives as a header criterion; answering it from the index keeps
	// Mail's most common search off the disk.
	for i := 0; i < len(criteria.Header); i++ {
		if !strings.EqualFold(criteria.Header[i].Key, "Subject") || criteria.Header[i].Value == "" {
			continue
		}
		ids, err := s.user.store.SearchColumn(s.ctx, sel.id, "subject", criteria.Header[i].Value)
		if err != nil {
			return nil, err
		}
		intersect(ids)
		// Answered: drop it so the header fallback does not re-read blobs.
		criteria.Header = append(criteria.Header[:i], criteria.Header[i+1:]...)
		i--
	}
	return result, nil
}

func (s *session) matches(m *message, seqNum uint32, criteria *imap.SearchCriteria, text textSet, flags map[*message][]imap.Flag) bool {
	if text != nil && !text[m.id] {
		return false
	}
	for _, seqSet := range criteria.SeqNum {
		if seqNum == 0 || !seqSet.Contains(seqNum) {
			return false
		}
	}
	for _, uidSet := range criteria.UID {
		if !uidSet.Contains(m.uid) {
			return false
		}
	}
	if !matchDate(m.internalDate, criteria.Since, criteria.Before) {
		return false
	}
	if criteria.Larger != 0 && m.size <= criteria.Larger {
		return false
	}
	if criteria.Smaller != 0 && m.size >= criteria.Smaller {
		return false
	}

	has := func(flag imap.Flag) bool {
		for _, f := range flags[m] {
			if f == flag {
				return true
			}
		}
		return false
	}
	for _, flag := range criteria.Flag {
		if !has(flag) {
			return false
		}
	}
	for _, flag := range criteria.NotFlag {
		if has(flag) {
			return false
		}
	}

	// Header and sent-date criteria need the message itself.
	if len(criteria.Header) > 0 || !criteria.SentSince.IsZero() || !criteria.SentBefore.IsZero() {
		header, ok := s.headerOf(m)
		if !ok {
			return false
		}
		for _, hc := range criteria.Header {
			if !matchHeaderField(header, hc.Key, hc.Value) {
				return false
			}
		}
		if !criteria.SentSince.IsZero() || !criteria.SentBefore.IsZero() {
			t, err := header.Date()
			if err != nil || !matchDate(t, criteria.SentSince, criteria.SentBefore) {
				return false
			}
		}
	}

	for i := range criteria.Not {
		if s.matches(m, seqNum, &criteria.Not[i], nil, flags) {
			return false
		}
	}
	for _, or := range criteria.Or {
		if !s.matches(m, seqNum, &or[0], nil, flags) && !s.matches(m, seqNum, &or[1], nil, flags) {
			return false
		}
	}
	return true
}

func (s *session) headerOf(m *message) (gomail.Header, bool) {
	raw, err := s.user.store.ReadBlob(m.blobHash)
	if err != nil {
		return gomail.Header{}, false
	}
	ent, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil && ent == nil {
		return gomail.Header{}, false
	}
	return gomail.Header{Header: ent.Header}, true
}

func matchHeaderField(h gomail.Header, key, pattern string) bool {
	fields := h.FieldsByKey(key)
	if pattern == "" {
		return fields.Len() > 0
	}
	pattern = strings.ToLower(pattern)
	for fields.Next() {
		v, err := fields.Text()
		if err != nil {
			v = fields.Value()
		}
		if strings.Contains(strings.ToLower(v), pattern) {
			return true
		}
	}
	return false
}

// matchDate compares dates with the time of day and zone discarded, as
// RFC 3501 requires for SINCE and BEFORE.
func matchDate(t, since, before time.Time) bool {
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if !since.IsZero() && t.Before(since) {
		return false
	}
	if !before.IsZero() && !t.Before(before) {
		return false
	}
	return true
}

// staticSearchCriteriaLocked resolves "*" and "$" inside search criteria, the
// same way a number set is resolved for FETCH.
func (sel *selectedMailbox) staticSearchCriteriaLocked(criteria *imap.SearchCriteria) {
	seqNums := make([]imap.SeqSet, 0, len(criteria.SeqNum))
	for _, set := range criteria.SeqNum {
		switch resolved := sel.staticNumSetLocked(set).(type) {
		case imap.SeqSet:
			seqNums = append(seqNums, resolved)
		case imap.UIDSet: // SEARCHRES substituted a UID set
			criteria.UID = append(criteria.UID, resolved)
		}
	}
	criteria.SeqNum = seqNums

	for i, set := range criteria.UID {
		if resolved, ok := sel.staticNumSetLocked(set).(imap.UIDSet); ok {
			criteria.UID[i] = resolved
		}
	}
	for i := range criteria.Not {
		sel.staticSearchCriteriaLocked(&criteria.Not[i])
	}
	for i := range criteria.Or {
		for j := range criteria.Or[i] {
			sel.staticSearchCriteriaLocked(&criteria.Or[i][j])
		}
	}
}

// headerOf parses just the header block, which is all ENVELOPE needs.
func headerOf(raw []byte) textproto.Header {
	h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return textproto.Header{}
	}
	return h
}
