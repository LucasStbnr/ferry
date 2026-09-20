// Package store is Ferry's source of truth. Resend models neither read state,
// folders, drafts nor deletion, so everything an IMAP client expects to persist
// lives here: mailboxes, per-mailbox UIDs, flags, tombstones and a full-text
// index. Message bodies are content-addressed blobs on disk.
//
// Every query is scoped to one account through Account, so an IMAP session can
// never observe another account's rows.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so cross-compilation stays trivial
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is bumped whenever schema.sql or a migration changes.
const schemaVersion = 1

// Common errors.
var (
	ErrNotFound      = errors.New("store: not found")
	ErrAlreadyExists = errors.New("store: already exists")
)

// DB is the Ferry database together with its blob directory.
type DB struct {
	sql     *sql.DB
	blobDir string
}

// Open creates or opens the database at dbPath and the blob tree at blobDir.
func Open(ctx context.Context, dbPath, blobDir string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("store: create data directory: %w", err)
	}
	if err := os.MkdirAll(blobDir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create blob directory: %w", err)
	}

	dsn := "file:" + dbPath + "?" +
		"_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=busy_timeout(10000)" +
		"&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", dbPath, err)
	}
	// modernc's driver serialises per connection; a small pool keeps writer
	// contention predictable while letting reads proceed under WAL.
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	sqlDB.SetConnMaxIdleTime(time.Minute)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: ping %s: %w", dbPath, err)
	}

	db := &DB{sql: sqlDB, blobDir: blobDir}
	if err := db.migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the database handle.
func (db *DB) Close() error { return db.sql.Close() }

// SQL exposes the underlying handle for maintenance commands.
func (db *DB) SQL() *sql.DB { return db.sql }

// BlobDir returns the root of the blob tree.
func (db *DB) BlobDir() string { return db.blobDir }

func (db *DB) migrate(ctx context.Context) error {
	var have int
	err := db.sql.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&have)
	switch {
	case err == nil:
	case isMissingTable(err) || errors.Is(err, sql.ErrNoRows):
		have = 0
	default:
		return fmt.Errorf("store: read schema version: %w", err)
	}

	if have == schemaVersion {
		return nil
	}
	if have > schemaVersion {
		return fmt.Errorf("store: database is schema version %d, this build understands %d; upgrade ferry", have, schemaVersion)
	}

	return db.tx(ctx, func(tx *sql.Tx) error {
		if have == 0 {
			if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
				return fmt.Errorf("store: apply schema: %w", err)
			}
		}
		// Future migrations run here, guarded by `if have < N`.
		_, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES ('schema_version', ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			strconv.Itoa(schemaVersion))
		return err
	})
}

// isMissingTable reports the error a fresh, empty database gives for a query
// against the meta table.
func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// tx runs fn inside a transaction, rolling back on error or panic.
func (db *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Vacuum compacts the database and truncates the WAL. It is the maintenance
// half of `ferry doctor --repair`.
func (db *DB) Vacuum(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	_, err := db.sql.ExecContext(ctx, `VACUUM`)
	return err
}

// Check runs SQLite's integrity check.
func (db *DB) Check(ctx context.Context) error {
	var result string
	if err := db.sql.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("store: integrity check failed: %s", result)
	}
	return nil
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// eachRow runs a query and calls fn for every row, always closing the result
// set — including when fn returns early or panics, which the explicit-Close
// form does not. A result set left open holds a connection, and on SQLite that
// means the next write on the same transaction blocks until the busy timeout.
func eachRow(ctx context.Context, q querier, query string, args []any, fn func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
