package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Sync directions.
const (
	KindReceived = "received"
	KindSent     = "sent"
)

// SyncState is where the poller left off for one direction of one account.
//
// Resend's list endpoints are cursor-paginated newest first, which gives two
// distinct walks: an incremental one from the newest row down to NewestID, and
// a backfill from BackfillCursor further into the past until the history runs
// out. Keeping both in one row makes a resumed backfill exact after a crash.
type SyncState struct {
	Kind string
	// NewestID is the most recent Resend id already stored; the incremental
	// pass stops when it sees it.
	NewestID string
	// BackfillCursor is the oldest id fetched so far; the backfill pass asks
	// for the page after it.
	BackfillCursor string
	// BackfillDone is set once the history has been walked to the end.
	BackfillDone bool
	LastSyncAt   time.Time
	LastError    string
}

// SyncState reads the state for one direction, returning a zero value when the
// account has never synced.
func (s *AccountStore) SyncState(ctx context.Context, kind string) (SyncState, error) {
	st := SyncState{Kind: kind}
	var (
		done   int
		lastAt int64
	)
	err := s.db.sql.QueryRowContext(ctx,
		`SELECT newest_id, backfill_cursor, backfill_done, last_sync_at, last_error
		 FROM sync_state WHERE account_id = ? AND kind = ?`, s.acct.ID, kind).
		Scan(&st.NewestID, &st.BackfillCursor, &done, &lastAt, &st.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.BackfillDone = done != 0
	if lastAt != 0 {
		st.LastSyncAt = time.Unix(lastAt, 0)
	}
	return st, nil
}

// SaveSyncState writes the state back.
func (s *AccountStore) SaveSyncState(ctx context.Context, st SyncState) error {
	done := 0
	if st.BackfillDone {
		done = 1
	}
	var lastAt int64
	if !st.LastSyncAt.IsZero() {
		lastAt = st.LastSyncAt.Unix()
	}
	_, err := s.db.sql.ExecContext(ctx,
		`INSERT INTO sync_state(account_id, kind, newest_id, backfill_cursor, backfill_done, last_sync_at, last_error)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(account_id, kind) DO UPDATE SET
		     newest_id       = excluded.newest_id,
		     backfill_cursor = excluded.backfill_cursor,
		     backfill_done   = excluded.backfill_done,
		     last_sync_at    = excluded.last_sync_at,
		     last_error      = excluded.last_error`,
		s.acct.ID, st.Kind, st.NewestID, st.BackfillCursor, done, lastAt, st.LastError)
	return err
}

// Counts summarises an account for `ferry status`.
type Counts struct {
	Messages   int64
	Unseen     int64
	Bytes      int64
	Tombstones int64
	Mailboxes  int64
}

// Counts returns account-wide totals.
func (s *AccountStore) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	err := s.db.sql.QueryRowContext(ctx,
		`SELECT
		     (SELECT count(*) FROM messages   WHERE account_id = ?),
		     (SELECT count(*) FROM messages m WHERE m.account_id = ?
		        AND NOT EXISTS (SELECT 1 FROM message_flags f WHERE f.message_id = m.id AND f.flag = '\Seen')),
		     (SELECT coalesce(sum(size), 0) FROM blobs      WHERE account_id = ?),
		     (SELECT count(*)               FROM tombstones WHERE account_id = ?),
		     (SELECT count(*)               FROM mailboxes  WHERE account_id = ?)`,
		s.acct.ID, s.acct.ID, s.acct.ID, s.acct.ID, s.acct.ID).
		Scan(&c.Messages, &c.Unseen, &c.Bytes, &c.Tombstones, &c.Mailboxes)
	return c, err
}
