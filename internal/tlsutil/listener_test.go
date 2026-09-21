package tlsutil_test

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LucasStbnr/ferry/internal/tlsutil"
)

// testListener returns a bounded listener on loopback with a generated bundle.
func testListener(t *testing.T, timeout time.Duration) (net.Listener, *tls.Config) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bundle, err := tlsutil.EnsureBundle(dir, []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	serverCfg, err := bundle.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	clientCfg, err := bundle.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tlsutil.NewListener(t.Context(), raw, serverCfg, timeout)
	t.Cleanup(func() { _ = ln.Close() })

	return ln, clientCfg
}

// TestListenerDropsSilentClient is the regression test for the hang that made
// a macOS profile install spin forever: a client that connects and then waits
// for the server to speak first must be hung up on, not humoured indefinitely.
func TestListenerDropsSilentClient(t *testing.T) {
	t.Parallel()
	ln, _ := testListener(t, 300*time.Millisecond)

	accepted := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		accepted <- err
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Say nothing, as accountsd does when probing for a plaintext greeting.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("expected the server to close the connection, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("server took %s to drop a silent client", elapsed)
	}

	// The connection never handshook, so it must never reach the server.
	select {
	case err := <-accepted:
		t.Fatalf("Accept returned for a connection that never handshook: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestListenerAcceptsTLSConn guards the type the servers see. go-smtp and
// go-imap both decide a connection is encrypted by asserting it is a
// *tls.Conn; if Accept ever returns a wrapper instead, both conclude the
// session is in the clear and refuse to authenticate.
func TestListenerAcceptsTLSConn(t *testing.T) {
	t.Parallel()
	ln, clientCfg := testListener(t, time.Second)

	type result struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		accepted <- result{c, err}
	}()

	client, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	select {
	case got := <-accepted:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.conn.Close()
		if _, ok := got.conn.(*tls.Conn); !ok {
			t.Fatalf("Accept returned %T, want *tls.Conn", got.conn)
		}
		// The handshake is already done, so the server can write first.
		if _, err := got.conn.Write([]byte("220 ready\r\n")); err != nil {
			t.Fatalf("write after handshake: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept did not return a handshaken connection")
	}

	buf := make([]byte, len("220 ready\r\n"))
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "220 ready\r\n" {
		t.Fatalf("got %q", buf)
	}
}

// TestListenerNoDeadlineAfterHandshake makes sure the handshake deadline is
// cleared. IMAP connections park in IDLE for many minutes and must not be
// killed by the deadline that bounded their handshake.
func TestListenerNoDeadlineAfterHandshake(t *testing.T) {
	t.Parallel()
	ln, clientCfg := testListener(t, 200*time.Millisecond)

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	client, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var server net.Conn
	select {
	case server = <-accepted:
		defer server.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("no connection accepted")
	}

	// Idle well past the handshake timeout, then write.
	time.Sleep(500 * time.Millisecond)
	if _, err := server.Write([]byte("still here\r\n")); err != nil {
		t.Fatalf("write after idling past the handshake timeout: %v", err)
	}
}

// TestListenerCloseUnblocksAccept checks shutdown does not leak the accept.
func TestListenerCloseUnblocksAccept(t *testing.T) {
	t.Parallel()
	ln, _ := testListener(t, time.Second)

	done := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Accept returned a connection after Close")
		}
		if !errors.Is(err, net.ErrClosed) {
			t.Logf("Accept returned %v", err) // any error is acceptable here
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Accept")
	}
}
