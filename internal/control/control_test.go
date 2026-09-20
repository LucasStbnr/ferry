package control_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LucasStbnr/ferry/internal/control"
)

type stubHandler struct {
	mu      sync.Mutex
	syncReq control.SyncRequest
	reloads int
	failErr error
}

func (h *stubHandler) Status(context.Context) (*control.Status, error) {
	if h.failErr != nil {
		return nil, h.failErr
	}
	return &control.Status{
		Version:  "test",
		Uptime:   "1m",
		IMAPAddr: "127.0.0.1:1993",
		Accounts: []control.AccountStatus{{Name: "acct", Messages: 7}},
	}, nil
}

func (h *stubHandler) Sync(_ context.Context, req control.SyncRequest) (*control.SyncResult, error) {
	h.mu.Lock()
	h.syncReq = req
	h.mu.Unlock()
	if h.failErr != nil {
		return nil, h.failErr
	}
	return &control.SyncResult{
		Accounts: []control.AccountSyncResult{{Account: "acct", Received: 3}},
	}, nil
}

func (h *stubHandler) Reload(context.Context) error {
	h.mu.Lock()
	h.reloads++
	h.mu.Unlock()
	return h.failErr
}

func serve(t *testing.T, h control.Handler) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), control.SocketName)
	srv := control.NewServer(path, h, nil)
	if err := srv.Listen(t.Context()); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	return path
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := &stubHandler{}
	path := serve(t, h)

	if !control.Available(ctx, path) {
		t.Fatal("the socket is not reachable")
	}
	c := control.Dial(path)

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Version != "test" || len(st.Accounts) != 1 || st.Accounts[0].Messages != 7 {
		t.Fatalf("status = %+v", st)
	}

	res, err := c.Sync(ctx, control.SyncRequest{Account: "acct", Backfill: true})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Accounts) != 1 || res.Accounts[0].Received != 3 {
		t.Fatalf("sync result = %+v", res)
	}
	h.mu.Lock()
	got := h.syncReq
	h.mu.Unlock()
	if got.Account != "acct" || !got.Backfill {
		t.Fatalf("the request did not reach the handler intact: %+v", got)
	}

	if err := c.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	h.mu.Lock()
	reloads := h.reloads
	h.mu.Unlock()
	if reloads != 1 {
		t.Fatalf("reloads = %d", reloads)
	}
}

func TestSocketIsPrivate(t *testing.T) {
	path := serve(t, &stubHandler{})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The filesystem is the whole authorisation model for this socket.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("socket is mode %04o; any user on this machine could drive the daemon", perm)
	}
}

func TestErrorsReachTheClient(t *testing.T) {
	h := &stubHandler{failErr: errors.New("something specific went wrong")}
	c := control.Dial(serve(t, h))

	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != "something specific went wrong" {
		t.Fatalf("error = %q; the daemon's message should survive the round trip", err)
	}
}

func TestNoDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), control.SocketName)
	if control.Available(t.Context(), path) {
		t.Fatal("Available reported a daemon where there is none")
	}
	_, err := control.Dial(path).Status(context.Background())
	if !errors.Is(err, control.ErrNoDaemon) {
		t.Fatalf("error = %v, want ErrNoDaemon", err)
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, control.SocketName)

	// A crashed daemon leaves the socket file behind with nothing listening.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if _, err := os.Create(path); err != nil {
		t.Fatal(err)
	}

	srv := control.NewServer(path, &stubHandler{}, nil)
	if err := srv.Listen(t.Context()); err != nil {
		t.Fatalf("a stale socket should be replaced, not fatal: %v", err)
	}
	defer srv.Close()
	go srv.Serve()

	if _, err := control.Dial(path).Status(context.Background()); err != nil {
		t.Fatalf("status after replacing a stale socket: %v", err)
	}
}

func TestSecondDaemonIsRefused(t *testing.T) {
	path := serve(t, &stubHandler{})

	// Two daemons on one data directory would corrupt each other's view of
	// the database, so the second must refuse to start.
	second := control.NewServer(path, &stubHandler{}, nil)
	if err := second.Listen(t.Context()); err == nil {
		second.Close()
		t.Fatal("a second daemon was allowed to take over the socket")
	}
}

func TestCloseRemovesTheSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), control.SocketName)
	srv := control.NewServer(path, &stubHandler{}, nil)
	if err := srv.Listen(t.Context()); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	time.Sleep(10 * time.Millisecond)

	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the socket file survived Close")
	}
}
