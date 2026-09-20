package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// Blobs are the full RFC 5322 messages. They are content-addressed by SHA-256,
// so the same message filed in two mailboxes (a COPY, or a Sent copy Mail also
// APPENDs) costs one file. The database holds a reference count; the file is
// removed when it reaches zero.

// HashBytes returns the hex SHA-256 of data, which is a blob's name.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// blobPath spreads blobs over 256 directories so no directory grows unwieldy.
func (s *AccountStore) blobPath(hash string) string {
	return filepath.Join(s.db.blobDir, strconv.FormatInt(s.acct.ID, 10), hash[:2], hash+".eml")
}

// writeBlob stores data on disk if it is not already there and returns its
// hash. Writing is atomic, so a crash never leaves a truncated message.
func (s *AccountStore) writeBlob(data []byte) (string, error) {
	hash := HashBytes(data)
	path := s.blobPath(hash)
	if fi, err := os.Stat(path); err == nil && fi.Size() == int64(len(data)) {
		return hash, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("store: create blob directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("store: store blob: %w", err)
	}
	return hash, nil
}

// ReadBlob returns the full message bytes for a hash.
func (s *AccountStore) ReadBlob(hash string) ([]byte, error) {
	data, err := os.ReadFile(s.blobPath(hash))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: blob %s", ErrNotFound, hash)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read blob %s: %w", hash, err)
	}
	return data, nil
}

// OpenBlob streams a message, for FETCH of a large body.
func (s *AccountStore) OpenBlob(hash string) (io.ReadSeekCloser, error) {
	f, err := os.Open(s.blobPath(hash))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: blob %s", ErrNotFound, hash)
	}
	if err != nil {
		return nil, fmt.Errorf("store: open blob %s: %w", hash, err)
	}
	return f, nil
}

// HasBlob reports whether the blob file exists on disk.
func (s *AccountStore) HasBlob(hash string) bool {
	_, err := os.Stat(s.blobPath(hash))
	return err == nil
}

func (s *AccountStore) refBlobTx(ctx context.Context, tx *sql.Tx, hash string, size int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO blobs(account_id, hash, size, refs) VALUES (?, ?, ?, 1)
		 ON CONFLICT(account_id, hash) DO UPDATE SET refs = refs + 1`,
		s.acct.ID, hash, size)
	return err
}

// unrefBlobsTx drops one reference from each hash and returns those that fell
// to zero, for the caller to delete from disk after the transaction commits.
func (s *AccountStore) unrefBlobsTx(ctx context.Context, tx *sql.Tx, hashes []string) ([]string, error) {
	var orphaned []string
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx,
			`UPDATE blobs SET refs = refs - 1 WHERE account_id = ? AND hash = ?`, s.acct.ID, h); err != nil {
			return nil, err
		}
		var refs int
		err := tx.QueryRowContext(ctx,
			`SELECT refs FROM blobs WHERE account_id = ? AND hash = ?`, s.acct.ID, h).Scan(&refs)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if refs <= 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM blobs WHERE account_id = ? AND hash = ?`, s.acct.ID, h); err != nil {
				return nil, err
			}
			orphaned = append(orphaned, h)
		}
	}
	return orphaned, nil
}

// removeBlobFiles deletes blob files that no longer have references. It runs
// after the transaction commits: losing a file for a row that still exists
// would be far worse than leaving a stray file behind.
func (s *AccountStore) removeBlobFiles(hashes []string) {
	for _, h := range hashes {
		_ = os.Remove(s.blobPath(h))
	}
}

// GCBlobs deletes blob files with no database reference, and reports how many
// bytes it freed. It repairs the leak a crash between commit and unlink leaves.
func (s *AccountStore) GCBlobs(ctx context.Context) (removed int, freed int64, err error) {
	root := filepath.Join(s.db.blobDir, strconv.FormatInt(s.acct.ID, 10))
	referenced := map[string]bool{}
	if err := eachRow(ctx, s.db.sql,
		`SELECT hash FROM blobs WHERE account_id = ?`, []any{s.acct.ID},
		func(rows *sql.Rows) error {
			var h string
			if err := rows.Scan(&h); err != nil {
				return err
			}
			referenced[h] = true
			return nil
		}); err != nil {
		return 0, 0, err
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".eml" {
			return nil
		}
		hash := d.Name()[:len(d.Name())-len(".eml")]
		if referenced[hash] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// The file went away while we were walking. Nothing to reclaim,
			// and nothing worth failing a best-effort sweep over.
			return nil //nolint:nilerr // deliberate: keep sweeping
		}
		if err := os.Remove(path); err == nil {
			removed++
			freed += info.Size()
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return removed, freed, walkErr
	}
	return removed, freed, nil
}
