package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Delim is the IMAP hierarchy separator Ferry exposes.
const Delim = '/'

// Standard mailbox names and their SPECIAL-USE attributes. Apple Mail uses
// SPECIAL-USE to bind its Sent/Drafts/Trash/Junk/Archive buttons to the right
// folder instead of guessing from names.
const (
	Inbox   = "INBOX"
	Sent    = "Sent"
	Drafts  = "Drafts"
	Archive = "Archive"
	Trash   = "Trash"
	Junk    = "Junk"
)

// DefaultMailboxes is the folder set created with every account, in the order
// Ferry creates them.
var DefaultMailboxes = []struct {
	Name       string
	SpecialUse string
}{
	{Inbox, ""},
	{Sent, `\Sent`},
	{Drafts, `\Drafts`},
	{Archive, `\Archive`},
	{Trash, `\Trash`},
	{Junk, `\Junk`},
}

// Mailbox is an IMAP folder belonging to one account.
type Mailbox struct {
	ID          int64
	AccountID   int64
	Name        string
	SpecialUse  string
	UIDValidity uint32
	UIDNext     uint32
	Subscribed  bool
}

// Account returns a handle scoped to one account. An IMAP or SMTP session
// holds exactly one, and therefore cannot reach another account's data.
func (db *DB) Account(a *Account) *AccountStore {
	return &AccountStore{db: db, acct: *a}
}

// AccountStore is a per-account view of the database.
type AccountStore struct {
	db   *DB
	acct Account
}

// ID returns the account's row id.
func (s *AccountStore) ID() int64 { return s.acct.ID }

// Name returns the account name.
func (s *AccountStore) Name() string { return s.acct.Name }

// Info returns a copy of the account record.
func (s *AccountStore) Info() Account { return s.acct }

// ValidateMailboxName rejects names IMAP clients cannot address.
func ValidateMailboxName(name string) error {
	switch {
	case name == "":
		return errors.New("store: empty mailbox name")
	case strings.ContainsAny(name, "\x00\r\n\"\\%*"):
		return fmt.Errorf("store: mailbox name %q contains a reserved character", name)
	case strings.HasPrefix(name, string(Delim)), strings.HasSuffix(name, string(Delim)):
		return fmt.Errorf("store: mailbox name %q must not start or end with %q", name, string(Delim))
	case strings.Contains(name, string([]byte{Delim, Delim})):
		return fmt.Errorf("store: mailbox name %q has an empty path element", name)
	case len(name) > 255:
		return fmt.Errorf("store: mailbox name is too long")
	}
	return nil
}

// canonicalMailbox folds the special-cased INBOX to upper case, as RFC 3501
// requires, and leaves every other name alone.
func canonicalMailbox(name string) string {
	if strings.EqualFold(name, Inbox) {
		return Inbox
	}
	return name
}

const mailboxCols = `id, account_id, name, special_use, uid_validity, uid_next, subscribed`

func scanMailbox(row interface{ Scan(...any) error }) (*Mailbox, error) {
	var m Mailbox
	var subscribed int
	if err := row.Scan(&m.ID, &m.AccountID, &m.Name, &m.SpecialUse, &m.UIDValidity, &m.UIDNext, &subscribed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m.Subscribed = subscribed != 0
	return &m, nil
}

// Mailbox returns one mailbox by name.
func (s *AccountStore) Mailbox(ctx context.Context, name string) (*Mailbox, error) {
	name = canonicalMailbox(name)
	row := s.db.sql.QueryRowContext(ctx,
		`SELECT `+mailboxCols+` FROM mailboxes WHERE account_id = ? AND name = ?`, s.acct.ID, name)
	m, err := scanMailbox(row)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: mailbox %q", ErrNotFound, name)
	}
	return m, err
}

// Mailboxes lists the account's mailboxes with INBOX first, then alphabetical.
func (s *AccountStore) Mailboxes(ctx context.Context) ([]Mailbox, error) {
	var out []Mailbox
	err := eachRow(ctx, s.db.sql,
		`SELECT `+mailboxCols+` FROM mailboxes WHERE account_id = ?
		 ORDER BY (name = 'INBOX') DESC, name`, []any{s.acct.ID},
		func(rows *sql.Rows) error {
			m, err := scanMailbox(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
			return nil
		})
	return out, err
}

// CreateMailbox adds a mailbox. Parent folders are created implicitly, so
// creating "Clients/Acme" also creates "Clients".
func (s *AccountStore) CreateMailbox(ctx context.Context, name, specialUse string) error {
	name = canonicalMailbox(name)
	if err := ValidateMailboxName(name); err != nil {
		return err
	}
	return s.db.tx(ctx, func(tx *sql.Tx) error {
		parts := strings.Split(name, string(Delim))
		for i := range parts {
			path := strings.Join(parts[:i+1], string(Delim))
			use := ""
			if i == len(parts)-1 {
				use = specialUse
			}
			err := s.createMailboxTx(ctx, tx, path, use)
			if errors.Is(err, ErrAlreadyExists) && i < len(parts)-1 {
				continue // an existing parent is fine
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *AccountStore) createMailboxTx(ctx context.Context, tx *sql.Tx, name, specialUse string) error {
	// UIDVALIDITY must change if a mailbox is deleted and recreated under the
	// same name, so that clients discard their cache. A second-resolution
	// clock is enough and keeps the value stable across restarts.
	uidValidity := uint32(time.Now().Unix())
	_, err := tx.ExecContext(ctx,
		`INSERT INTO mailboxes(account_id, name, special_use, uid_validity, uid_next, subscribed)
		 VALUES (?, ?, ?, ?, 1, 1)`,
		s.acct.ID, name, specialUse, uidValidity)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("%w: mailbox %q", ErrAlreadyExists, name)
		}
		return err
	}
	return nil
}

// EnsureDefaultMailboxes creates any missing standard folder. It is safe to
// call on every start.
func (s *AccountStore) EnsureDefaultMailboxes(ctx context.Context) error {
	for _, d := range DefaultMailboxes {
		err := s.CreateMailbox(ctx, d.Name, d.SpecialUse)
		if err != nil && !errors.Is(err, ErrAlreadyExists) {
			return err
		}
	}
	return nil
}

// DeleteMailbox removes a mailbox and tombstones every message in it, so that
// a later sync does not resurrect the contents.
func (s *AccountStore) DeleteMailbox(ctx context.Context, name string) error {
	name = canonicalMailbox(name)
	if name == Inbox {
		return errors.New("store: INBOX cannot be deleted")
	}
	mbox, err := s.Mailbox(ctx, name)
	if err != nil {
		return err
	}
	return s.db.tx(ctx, func(tx *sql.Tx) error {
		var children int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM mailboxes WHERE account_id = ? AND name LIKE ? ESCAPE '\'`,
			s.acct.ID, likePrefix(name)+string(Delim)+"%").Scan(&children); err != nil {
			return err
		}
		if children > 0 {
			return fmt.Errorf("store: mailbox %q has child mailboxes", name)
		}
		if err := s.tombstoneMailboxTx(ctx, tx, mbox.ID); err != nil {
			return err
		}
		if err := s.dropMessagesTx(ctx, tx, mbox.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM mailboxes WHERE id = ?`, mbox.ID)
		return err
	})
}

// RenameMailbox renames a mailbox and any descendants beneath it.
func (s *AccountStore) RenameMailbox(ctx context.Context, oldName, newName string) error {
	oldName, newName = canonicalMailbox(oldName), canonicalMailbox(newName)
	if err := ValidateMailboxName(newName); err != nil {
		return err
	}
	if oldName == Inbox {
		// RFC 3501 gives INBOX rename a special meaning (move the messages out
		// and keep the folder). Ferry does not implement it rather than doing
		// something surprising.
		return errors.New("store: renaming INBOX is not supported")
	}
	return s.db.tx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM mailboxes WHERE account_id = ? AND name = ?`, s.acct.ID, newName).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return fmt.Errorf("%w: mailbox %q", ErrAlreadyExists, newName)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE mailboxes SET name = ? WHERE account_id = ? AND name = ?`, newName, s.acct.ID, oldName)
		if err != nil {
			return err
		}
		if err := mustAffect(res, fmt.Errorf("%w: mailbox %q", ErrNotFound, oldName)); err != nil {
			return err
		}
		// Descendants keep their position under the new parent.
		_, err = tx.ExecContext(ctx,
			`UPDATE mailboxes SET name = ? || substr(name, ?) 
			 WHERE account_id = ? AND name LIKE ? ESCAPE '\'`,
			newName, len(oldName)+1, s.acct.ID, likePrefix(oldName)+string(Delim)+"%")
		return err
	})
}

// SetSubscribed records an IMAP SUBSCRIBE or UNSUBSCRIBE.
func (s *AccountStore) SetSubscribed(ctx context.Context, name string, subscribed bool) error {
	name = canonicalMailbox(name)
	v := 0
	if subscribed {
		v = 1
	}
	res, err := s.db.sql.ExecContext(ctx,
		`UPDATE mailboxes SET subscribed = ? WHERE account_id = ? AND name = ?`, v, s.acct.ID, name)
	if err != nil {
		return err
	}
	return mustAffect(res, fmt.Errorf("%w: mailbox %q", ErrNotFound, name))
}

// likePrefix escapes the LIKE wildcards in a literal prefix.
func likePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
