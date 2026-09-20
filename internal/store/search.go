package store

import (
	"context"
	"database/sql"
	"strings"
	"unicode"
)

// Full-text search backs IMAP SEARCH TEXT/BODY/SUBJECT. The index is a
// contentless FTS5 table whose rowid mirrors messages.id, so it costs the
// tokens and nothing else; matching rows are joined back to messages.

// maxIndexedBody caps how much of a body is indexed. A newsletter with a
// megabyte of inlined HTML would otherwise dominate the index for no gain.
const maxIndexedBody = 256 << 10

func indexTx(ctx context.Context, tx *sql.Tx, id int64, subject, addrs, body string) error {
	if len(body) > maxIndexedBody {
		body = body[:maxIndexedBody]
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO message_fts(rowid, subject, addrs, body) VALUES (?, ?, ?, ?)`,
		id, subject, addrs, body)
	return err
}

// unindexTx removes a row. A contentless FTS5 table needs the original column
// values to delete cleanly, which Ferry does not keep, so it uses the
// 'delete-all'-free path: 'delete' with empty columns leaves the index
// slightly larger than necessary but never returns a stale row, because every
// hit is joined against messages before being returned.
func unindexTx(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO message_fts(message_fts, rowid, subject, addrs, body)
		 VALUES ('delete', ?, '', '', '')`, id)
	return err
}

func copyIndexTx(ctx context.Context, tx *sql.Tx, srcID, dstID int64) error {
	// A contentless index cannot be read back, so re-index the copy from the
	// messages row. The body text is lost for the copy; headers still match,
	// and a COPY is rare compared with the cost of keeping a second copy of
	// every body.
	var subject, from, to string
	err := tx.QueryRowContext(ctx,
		`SELECT subject, from_addr, to_addr FROM messages WHERE id = ?`, srcID).Scan(&subject, &from, &to)
	if err != nil {
		return err
	}
	return indexTx(ctx, tx, dstID, subject, from+" "+to, "")
}

// Search returns the ids of messages in a mailbox matching a free-text query,
// ordered by UID. The query is user input from an IMAP client, so it is
// quoted into an FTS5 phrase rather than interpreted as FTS5 syntax.
func (s *AccountStore) Search(ctx context.Context, mailboxID int64, query string) ([]int64, error) {
	phrase := ftsPhrase(query)
	if phrase == "" {
		return nil, nil
	}
	return s.searchIDs(ctx, mailboxID, phrase)
}

// SearchColumn restricts a query to one indexed column: "subject", "addrs" or
// "body".
func (s *AccountStore) SearchColumn(ctx context.Context, mailboxID int64, column, query string) ([]int64, error) {
	switch column {
	case "subject", "addrs", "body":
	default:
		return nil, nil
	}
	phrase := ftsPhrase(query)
	if phrase == "" {
		return nil, nil
	}
	return s.searchIDs(ctx, mailboxID, column+" : "+phrase)
}

// searchIDs runs an FTS5 match and returns the matching message ids in UID
// order. The join against messages is what keeps a stale index row (one whose
// message has since been expunged) from ever reaching a client.
func (s *AccountStore) searchIDs(ctx context.Context, mailboxID int64, match string) ([]int64, error) {
	var out []int64
	err := eachRow(ctx, s.db.sql,
		`SELECT m.id FROM message_fts f
		 JOIN messages m ON m.id = f.rowid
		 WHERE f.message_fts MATCH ? AND m.account_id = ? AND m.mailbox_id = ?
		 ORDER BY m.uid`,
		[]any{match, s.acct.ID, mailboxID},
		func(rows *sql.Rows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
			return nil
		})
	return out, err
}

// ftsPhrase turns arbitrary user text into a safe FTS5 phrase query. Every
// token is double-quoted, so FTS5 operators typed by the user are matched
// literally instead of changing the meaning of the query.
func ftsPhrase(query string) string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(fields) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		quoted = append(quoted, `"`+f+`"`)
	}
	// Adjacent quoted tokens form a phrase; joining with AND matches messages
	// containing all of them, which is what a mail client's user expects.
	return strings.Join(quoted, " AND ")
}
