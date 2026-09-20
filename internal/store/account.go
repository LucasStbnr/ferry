package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Account is one Resend account, exposed as one IMAP/SMTP login.
type Account struct {
	ID           int64
	Name         string
	Address      string
	Domains      []string
	PasswordHash string
	CreatedAt    time.Time
}

// nameRE constrains account names: they end up in file paths, IMAP logins and
// mobileconfig identifiers, so keep them boring.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidateName reports whether name is usable as an account name.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("store: invalid account name %q: use lower-case letters, digits, dot, dash or underscore (max 63)", name)
	}
	return nil
}

// CreateAccount registers a new account.
func (db *DB) CreateAccount(ctx context.Context, name, address, passwordHash string) (*Account, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	now := time.Now()
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO accounts(name, address, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		name, address, passwordHash, now.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("%w: account %q", ErrAlreadyExists, name)
		}
		return nil, fmt.Errorf("store: create account: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Account{ID: id, Name: name, Address: address, PasswordHash: passwordHash, CreatedAt: now}, nil
}

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var (
		a       Account
		domains string
		created int64
	)
	if err := row.Scan(&a.ID, &a.Name, &a.Address, &domains, &a.PasswordHash, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.CreatedAt = time.Unix(created, 0)
	a.Domains = strings.Fields(domains)
	return &a, nil
}

const accountCols = `id, name, address, domains, password_hash, created_at`

// AccountByName looks an account up, returning ErrNotFound if it is absent.
func (db *DB) AccountByName(ctx context.Context, name string) (*Account, error) {
	row := db.sql.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE name = ?`, name)
	a, err := scanAccount(row)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: account %q", ErrNotFound, name)
	}
	return a, err
}

// Accounts lists every account, ordered by name.
func (db *DB) Accounts(ctx context.Context) ([]Account, error) {
	var out []Account
	err := eachRow(ctx, db.sql, `SELECT `+accountCols+` FROM accounts ORDER BY name`, nil,
		func(rows *sql.Rows) error {
			a, err := scanAccount(rows)
			if err != nil {
				return err
			}
			out = append(out, *a)
			return nil
		})
	return out, err
}

// SetPasswordHash replaces the stored app-password hash.
func (db *DB) SetPasswordHash(ctx context.Context, name, hash string) error {
	res, err := db.sql.ExecContext(ctx, `UPDATE accounts SET password_hash = ? WHERE name = ?`, hash, name)
	if err != nil {
		return err
	}
	return mustAffect(res, fmt.Errorf("%w: account %q", ErrNotFound, name))
}

// SetDomains records the account's verified sending domains, which the SMTP
// server checks the From header against.
func (db *DB) SetDomains(ctx context.Context, name string, domains []string) error {
	norm := make([]string, 0, len(domains))
	seen := map[string]bool{}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		norm = append(norm, d)
	}
	sort.Strings(norm)
	res, err := db.sql.ExecContext(ctx, `UPDATE accounts SET domains = ? WHERE name = ?`, strings.Join(norm, " "), name)
	if err != nil {
		return err
	}
	return mustAffect(res, fmt.Errorf("%w: account %q", ErrNotFound, name))
}

// SetAddress records the account's primary From address.
func (db *DB) SetAddress(ctx context.Context, name, address string) error {
	res, err := db.sql.ExecContext(ctx, `UPDATE accounts SET address = ? WHERE name = ?`, address, name)
	if err != nil {
		return err
	}
	return mustAffect(res, fmt.Errorf("%w: account %q", ErrNotFound, name))
}

// DeleteAccount removes an account and every row that hangs off it. Blob files
// are left to Account.GC, which the caller runs afterwards.
func (db *DB) DeleteAccount(ctx context.Context, name string) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM accounts WHERE name = ?`, name)
	if err != nil {
		return err
	}
	return mustAffect(res, fmt.Errorf("%w: account %q", ErrNotFound, name))
}

func mustAffect(res sql.Result, notFound error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound
	}
	return nil
}
