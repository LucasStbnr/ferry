package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Message is a stored message's metadata. The body lives in the blob named by
// BlobHash and is read on demand.
type Message struct {
	ID           int64
	MailboxID    int64
	UID          uint32
	ResendID     string
	MessageID    string
	BlobHash     string
	Size         int64
	InternalDate time.Time
	SentDate     time.Time
	Subject      string
	From         string
	To           string
	Flags        []string
}

// NewMessage is a message being filed into a mailbox.
type NewMessage struct {
	// Raw is the complete RFC 5322 message.
	Raw []byte
	// ResendID links the message back to the Resend API, so that a later sync
	// recognises it and does not file a second copy. Empty for drafts.
	ResendID string
	// MessageID is the RFC 5322 Message-ID, used to dedupe a message that
	// arrives both from the API and from the client via APPEND.
	MessageID string
	// InternalDate is the IMAP arrival time. Defaults to now.
	InternalDate time.Time
	// SentDate is the Date header, used by SEARCH SENTSINCE.
	SentDate time.Time
	Subject  string
	From     string
	To       string
	// SearchText is the plain-text body indexed for full-text SEARCH.
	SearchText string
	// Flags are the initial flags.
	Flags []string
}

// CanonicalFlag normalises a flag: system flags (those starting with a
// backslash) are case-insensitive per RFC 3501, keywords are kept verbatim.
func CanonicalFlag(flag string) string {
	if !strings.HasPrefix(flag, `\`) {
		return flag
	}
	switch strings.ToLower(flag) {
	case `\seen`:
		return `\Seen`
	case `\answered`:
		return `\Answered`
	case `\flagged`:
		return `\Flagged`
	case `\deleted`:
		return `\Deleted`
	case `\draft`:
		return `\Draft`
	case `\recent`:
		return `\Recent`
	}
	return flag
}

func canonicalFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	seen := map[string]bool{}
	for _, f := range flags {
		f = CanonicalFlag(f)
		// \Recent is session state, not something to persist.
		if f == "" || f == `\Recent` || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

const messageCols = `id, mailbox_id, uid, resend_id, message_id, blob_hash, size,
	internal_date, sent_date, subject, from_addr, to_addr`

func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	var (
		m        Message
		internal int64
		sent     int64
	)
	err := row.Scan(&m.ID, &m.MailboxID, &m.UID, &m.ResendID, &m.MessageID, &m.BlobHash,
		&m.Size, &internal, &sent, &m.Subject, &m.From, &m.To)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m.InternalDate = time.Unix(internal, 0)
	if sent != 0 {
		m.SentDate = time.Unix(sent, 0)
	}
	return &m, nil
}

// Append files a message into a mailbox and returns the assigned UID. The blob
// is written first, then the row: a crash in between leaves an unreferenced
// file that GCBlobs reclaims, never a row without a body.
func (s *AccountStore) Append(ctx context.Context, mailboxID int64, nm *NewMessage) (uint32, error) {
	if len(nm.Raw) == 0 {
		return 0, errors.New("store: refusing to append an empty message")
	}
	hash, err := s.writeBlob(nm.Raw)
	if err != nil {
		return 0, err
	}

	internal := nm.InternalDate
	if internal.IsZero() {
		internal = time.Now()
	}
	var sentUnix int64
	if !nm.SentDate.IsZero() {
		sentUnix = nm.SentDate.Unix()
	}
	flags := canonicalFlags(nm.Flags)

	var uid uint32
	err = s.db.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`UPDATE mailboxes SET uid_next = uid_next + 1
			 WHERE id = ? AND account_id = ?
			 RETURNING uid_next - 1`, mailboxID, s.acct.ID).Scan(&uid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: mailbox %d", ErrNotFound, mailboxID)
			}
			return err
		}

		res, err := tx.ExecContext(ctx,
			`INSERT INTO messages(account_id, mailbox_id, uid, resend_id, message_id, blob_hash,
			     size, internal_date, sent_date, subject, from_addr, to_addr)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.acct.ID, mailboxID, uid, nm.ResendID, nm.MessageID, hash,
			len(nm.Raw), internal.Unix(), sentUnix, nm.Subject, nm.From, nm.To)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if err := s.refBlobTx(ctx, tx, hash, int64(len(nm.Raw))); err != nil {
			return err
		}
		if err := setFlagsTx(ctx, tx, id, flags); err != nil {
			return err
		}
		return indexTx(ctx, tx, id, nm.Subject, nm.From+" "+nm.To, nm.SearchText)
	})
	if err != nil {
		return 0, err
	}
	return uid, nil
}

// Messages lists a mailbox in UID order with flags attached.
func (s *AccountStore) Messages(ctx context.Context, mailboxID int64) ([]Message, error) {
	var (
		out []Message
		ids []int64
	)
	err := eachRow(ctx, s.db.sql,
		`SELECT `+messageCols+` FROM messages
		 WHERE account_id = ? AND mailbox_id = ? ORDER BY uid`, []any{s.acct.ID, mailboxID},
		func(rows *sql.Rows) error {
			m, err := scanMessage(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
			ids = append(ids, m.ID)
			return nil
		})
	if err != nil {
		return nil, err
	}

	flags, err := s.flagsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Flags = flags[out[i].ID]
	}
	return out, nil
}

// Message returns one message by id.
func (s *AccountStore) Message(ctx context.Context, id int64) (*Message, error) {
	row := s.db.sql.QueryRowContext(ctx,
		`SELECT `+messageCols+` FROM messages WHERE account_id = ? AND id = ?`, s.acct.ID, id)
	m, err := scanMessage(row)
	if err != nil {
		return nil, err
	}
	flags, err := s.flagsFor(ctx, []int64{id})
	if err != nil {
		return nil, err
	}
	m.Flags = flags[id]
	return m, nil
}

func (s *AccountStore) flagsFor(ctx context.Context, ids []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// Chunk to stay under SQLite's variable limit on very large mailboxes.
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		batch := ids[start:end]
		q := `SELECT message_id, flag FROM message_flags WHERE message_id IN (` +
			placeholders(len(batch)) + `)`
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		err := eachRow(ctx, s.db.sql, q, args, func(rows *sql.Rows) error {
			var id int64
			var flag string
			if err := rows.Scan(&id, &flag); err != nil {
				return err
			}
			out[id] = append(out[id], flag)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out, nil
}

// FlagsOp selects how StoreFlags combines flags with the existing set.
type FlagsOp int

// Flag store operations, matching IMAP STORE.
const (
	FlagsSet FlagsOp = iota
	FlagsAdd
	FlagsDel
)

// StoreFlags applies an IMAP STORE to a set of messages and returns the
// resulting flags per message id.
func (s *AccountStore) StoreFlags(ctx context.Context, ids []int64, op FlagsOp, flags []string) (map[int64][]string, error) {
	flags = canonicalFlags(flags)
	err := s.db.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			switch op {
			case FlagsSet:
				if _, err := tx.ExecContext(ctx, `DELETE FROM message_flags WHERE message_id = ?`, id); err != nil {
					return err
				}
				if err := setFlagsTx(ctx, tx, id, flags); err != nil {
					return err
				}
			case FlagsAdd:
				if err := setFlagsTx(ctx, tx, id, flags); err != nil {
					return err
				}
			case FlagsDel:
				for _, f := range flags {
					if _, err := tx.ExecContext(ctx,
						`DELETE FROM message_flags WHERE message_id = ? AND flag = ?`, id, f); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.flagsFor(ctx, ids)
}

func setFlagsTx(ctx context.Context, tx *sql.Tx, id int64, flags []string) error {
	for _, f := range flags {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_flags(message_id, flag) VALUES (?, ?)
			 ON CONFLICT DO NOTHING`, id, f); err != nil {
			return err
		}
	}
	return nil
}

// CopyResult pairs a source UID with the UID the copy received.
type CopyResult struct {
	SourceUID uint32
	DestUID   uint32
}

// Copy duplicates messages into another mailbox, reusing the same blob. The
// copies keep their flags, as RFC 3501 requires.
func (s *AccountStore) Copy(ctx context.Context, ids []int64, destMailboxID int64) ([]CopyResult, error) {
	var out []CopyResult
	err := s.db.tx(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		for _, id := range ids {
			row := tx.QueryRowContext(ctx,
				`SELECT `+messageCols+` FROM messages WHERE account_id = ? AND id = ?`, s.acct.ID, id)
			m, err := scanMessage(row)
			if err != nil {
				return err
			}

			var uid uint32
			if err := tx.QueryRowContext(ctx,
				`UPDATE mailboxes SET uid_next = uid_next + 1
				 WHERE id = ? AND account_id = ? RETURNING uid_next - 1`,
				destMailboxID, s.acct.ID).Scan(&uid); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: mailbox %d", ErrNotFound, destMailboxID)
				}
				return err
			}

			var sentUnix int64
			if !m.SentDate.IsZero() {
				sentUnix = m.SentDate.Unix()
			}
			res, err := tx.ExecContext(ctx,
				`INSERT INTO messages(account_id, mailbox_id, uid, resend_id, message_id, blob_hash,
				     size, internal_date, sent_date, subject, from_addr, to_addr)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				s.acct.ID, destMailboxID, uid, m.ResendID, m.MessageID, m.BlobHash,
				m.Size, m.InternalDate.Unix(), sentUnix, m.Subject, m.From, m.To)
			if err != nil {
				return err
			}
			newID, err := res.LastInsertId()
			if err != nil {
				return err
			}
			if err := s.refBlobTx(ctx, tx, m.BlobHash, m.Size); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO message_flags(message_id, flag)
				 SELECT ?, flag FROM message_flags WHERE message_id = ?`, newID, id); err != nil {
				return err
			}
			if err := copyIndexTx(ctx, tx, id, newID); err != nil {
				return err
			}
			out = append(out, CopyResult{SourceUID: m.UID, DestUID: uid})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Move relocates messages to another mailbox, giving them fresh UIDs there.
// Because the blob is unchanged this is a metadata-only operation.
func (s *AccountStore) Move(ctx context.Context, ids []int64, destMailboxID int64) ([]CopyResult, error) {
	var out []CopyResult
	err := s.db.tx(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		for _, id := range ids {
			var srcUID uint32
			if err := tx.QueryRowContext(ctx,
				`SELECT uid FROM messages WHERE account_id = ? AND id = ?`, s.acct.ID, id).Scan(&srcUID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: message %d", ErrNotFound, id)
				}
				return err
			}
			var uid uint32
			if err := tx.QueryRowContext(ctx,
				`UPDATE mailboxes SET uid_next = uid_next + 1
				 WHERE id = ? AND account_id = ? RETURNING uid_next - 1`,
				destMailboxID, s.acct.ID).Scan(&uid); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: mailbox %d", ErrNotFound, destMailboxID)
				}
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE messages SET mailbox_id = ?, uid = ? WHERE account_id = ? AND id = ?`,
				destMailboxID, uid, s.acct.ID, id); err != nil {
				return err
			}
			out = append(out, CopyResult{SourceUID: srcUID, DestUID: uid})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Expunge removes messages by id, writing a tombstone for each one that came
// from Resend so that the next sync does not bring it back. Resend itself is
// never asked to delete anything.
func (s *AccountStore) Expunge(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	var orphaned []string
	err := s.db.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		var hashes []string
		for _, id := range ids {
			var resendID, msgID, hash string
			err := tx.QueryRowContext(ctx,
				`SELECT resend_id, message_id, blob_hash FROM messages WHERE account_id = ? AND id = ?`,
				s.acct.ID, id).Scan(&resendID, &msgID, &hash)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			hashes = append(hashes, hash)
			if resendID != "" {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO tombstones(account_id, resend_id, message_id, deleted_at)
					 VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
					s.acct.ID, resendID, msgID, now); err != nil {
					return err
				}
			}
			if err := unindexTx(ctx, tx, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id); err != nil {
				return err
			}
		}
		var err error
		orphaned, err = s.unrefBlobsTx(ctx, tx, hashes)
		return err
	})
	if err != nil {
		return err
	}
	s.removeBlobFiles(orphaned)
	return nil
}

// tombstoneMailboxTx records a tombstone for every Resend-backed message in a
// mailbox that is about to disappear.
func (s *AccountStore) tombstoneMailboxTx(ctx context.Context, tx *sql.Tx, mailboxID int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO tombstones(account_id, resend_id, message_id, deleted_at)
		 SELECT account_id, resend_id, message_id, ?
		 FROM messages WHERE account_id = ? AND mailbox_id = ? AND resend_id <> ''
		 ON CONFLICT DO NOTHING`,
		time.Now().Unix(), s.acct.ID, mailboxID)
	return err
}

// dropMessagesTx deletes every message in a mailbox and its index rows. Blob
// files are left to GCBlobs.
func (s *AccountStore) dropMessagesTx(ctx context.Context, tx *sql.Tx, mailboxID int64) error {
	var (
		ids    []int64
		hashes []string
	)
	// The result set must be fully drained and closed before the deletes
	// below run on the same transaction.
	err := eachRow(ctx, tx,
		`SELECT id, blob_hash FROM messages WHERE account_id = ? AND mailbox_id = ?`,
		[]any{s.acct.ID, mailboxID},
		func(rows *sql.Rows) error {
			var id int64
			var hash string
			if err := rows.Scan(&id, &hash); err != nil {
				return err
			}
			ids = append(ids, id)
			hashes = append(hashes, hash)
			return nil
		})
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := unindexTx(ctx, tx, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages WHERE account_id = ? AND mailbox_id = ?`, s.acct.ID, mailboxID); err != nil {
		return err
	}
	_, err = s.unrefBlobsTx(ctx, tx, hashes)
	return err
}

// Tombstoned reports whether a Resend message id was deleted locally.
func (s *AccountStore) Tombstoned(ctx context.Context, resendID string) (bool, error) {
	var n int
	err := s.db.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM tombstones WHERE account_id = ? AND resend_id = ?`,
		s.acct.ID, resendID).Scan(&n)
	return n > 0, err
}

// FindByResendID returns the message with the given Resend id, in any mailbox.
func (s *AccountStore) FindByResendID(ctx context.Context, resendID string) (*Message, error) {
	row := s.db.sql.QueryRowContext(ctx,
		`SELECT `+messageCols+` FROM messages WHERE account_id = ? AND resend_id = ? LIMIT 1`,
		s.acct.ID, resendID)
	return scanMessage(row)
}

// FindByMessageID returns the message with the given RFC 5322 Message-ID.
// Sync uses it so that a message the client already APPENDed to Sent is not
// filed a second time when Resend reports it.
func (s *AccountStore) FindByMessageID(ctx context.Context, messageID string) (*Message, error) {
	if messageID == "" {
		return nil, ErrNotFound
	}
	row := s.db.sql.QueryRowContext(ctx,
		`SELECT `+messageCols+` FROM messages WHERE account_id = ? AND message_id = ? LIMIT 1`,
		s.acct.ID, messageID)
	return scanMessage(row)
}

// LinkResendID attaches a Resend id to a message that was stored without one,
// which is how a Sent copy the client uploaded is matched to the API record.
func (s *AccountStore) LinkResendID(ctx context.Context, id int64, resendID string) error {
	_, err := s.db.sql.ExecContext(ctx,
		`UPDATE messages SET resend_id = ? WHERE account_id = ? AND id = ?`, resendID, s.acct.ID, id)
	return err
}

// MailboxStats is the counting half of IMAP STATUS.
type MailboxStats struct {
	Messages uint32
	Unseen   uint32
	Deleted  uint32
	Size     int64
}

// Stats counts a mailbox without loading it.
func (s *AccountStore) Stats(ctx context.Context, mailboxID int64) (MailboxStats, error) {
	var st MailboxStats
	err := s.db.sql.QueryRowContext(ctx,
		`SELECT
		     count(*),
		     coalesce(sum(size), 0),
		     coalesce(sum(NOT EXISTS (SELECT 1 FROM message_flags f WHERE f.message_id = m.id AND f.flag = '\Seen')), 0),
		     coalesce(sum(    EXISTS (SELECT 1 FROM message_flags f WHERE f.message_id = m.id AND f.flag = '\Deleted')), 0)
		 FROM messages m WHERE m.account_id = ? AND m.mailbox_id = ?`,
		s.acct.ID, mailboxID).Scan(&st.Messages, &st.Size, &st.Unseen, &st.Deleted)
	return st, err
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}
