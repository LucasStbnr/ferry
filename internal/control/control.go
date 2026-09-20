// Package control is the private channel between the ferry CLI and a running
// daemon.
//
// `ferry sync` and `ferry status` have to reach the process that holds the
// database, not open a second copy of it. The channel is a Unix socket in the
// data directory with owner-only permissions, so authorisation is the
// filesystem's job and there are no credentials to manage. It speaks HTTP over
// that socket purely to get well-tested routing and framing; nothing listens
// on a network port.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// SocketName is the socket's file name inside the data directory.
const SocketName = "ferry.sock"

// Status is what `ferry status` renders.
type Status struct {
	Version   string          `json:"version"`
	StartedAt time.Time       `json:"started_at"`
	Uptime    string          `json:"uptime"`
	IMAPAddr  string          `json:"imap_addr"`
	SMTPAddr  string          `json:"smtp_addr"`
	Webhook   string          `json:"webhook_addr,omitempty"`
	Accounts  []AccountStatus `json:"accounts"`
}

// AccountStatus is one account's state.
type AccountStatus struct {
	Name            string    `json:"name"`
	Address         string    `json:"address"`
	Domains         []string  `json:"domains"`
	Messages        int64     `json:"messages"`
	Unseen          int64     `json:"unseen"`
	Bytes           int64     `json:"bytes"`
	Tombstones      int64     `json:"tombstones"`
	LastSync        time.Time `json:"last_sync,omitempty"`
	BackfillDone    bool      `json:"backfill_done"`
	LastError       string    `json:"last_error,omitempty"`
	WebhooksEnabled bool      `json:"webhooks_enabled"`
}

// SyncRequest asks the daemon to sync.
type SyncRequest struct {
	// Account limits the sync to one account; empty means all of them.
	Account string `json:"account,omitempty"`
	// Backfill resets the history cursors first, so the whole archive is
	// walked again.
	Backfill bool `json:"backfill,omitempty"`
}

// SyncResult reports what a sync did.
type SyncResult struct {
	Accounts []AccountSyncResult `json:"accounts"`
}

// AccountSyncResult is one account's outcome.
type AccountSyncResult struct {
	Account         string   `json:"account"`
	Received        int      `json:"received"`
	Sent            int      `json:"sent"`
	BackfillPending bool     `json:"backfill_pending"`
	Errors          []string `json:"errors,omitempty"`
}

// Handler is what the daemon implements to answer the CLI.
type Handler interface {
	Status(ctx context.Context) (*Status, error)
	Sync(ctx context.Context, req SyncRequest) (*SyncResult, error)
	Reload(ctx context.Context) error
}

// Server exposes a Handler on a Unix socket.
type Server struct {
	path string
	log  *slog.Logger
	http *http.Server
	ln   net.Listener
}

// NewServer prepares a control server at path.
func NewServer(path string, h Handler, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		st, err := h.Status(r.Context())
		writeResult(w, st, err)
	})
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		var req SyncRequest
		if r.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
				writeResult(w, nil, fmt.Errorf("control: malformed request: %w", err))
				return
			}
		}
		res, err := h.Sync(r.Context(), req)
		writeResult(w, res, err)
	})
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) {
		writeResult(w, map[string]string{"status": "ok"}, h.Reload(r.Context()))
	})

	return &Server{
		path: path,
		log:  log,
		http: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
}

// Listen creates the socket. A socket left behind by a crashed daemon is
// replaced, but only after checking that nothing is listening on it, so two
// daemons cannot quietly fight over one data directory.
func (s *Server) Listen(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(s.path); err == nil {
		if Available(ctx, s.path) {
			return fmt.Errorf("control: another ferry daemon is already listening on %s", s.path)
		}
		if err := os.Remove(s.path); err != nil {
			return fmt.Errorf("control: remove stale socket: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", s.path)
	if err != nil {
		return fmt.Errorf("control: listen on %s: %w", s.path, err)
	}
	// The socket is the whole authorisation model, so it must not be
	// group- or world-accessible.
	if err := os.Chmod(s.path, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	s.ln = ln
	return nil
}

// Serve answers requests until the server is closed.
func (s *Server) Serve() error {
	if s.ln == nil {
		return errors.New("control: Listen was not called")
	}
	err := s.http.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the server and removes the socket.
func (s *Server) Close() error {
	err := s.http.Close()
	_ = os.Remove(s.path)
	return err
}

func writeResult(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}
