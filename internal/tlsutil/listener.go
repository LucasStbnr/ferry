package tlsutil

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"time"
)

// HandshakeTimeout bounds how long a client may take to complete its TLS
// handshake once it has connected.
//
// A handshake on loopback costs under a millisecond and one across a slow
// mobile network a few seconds, so this is generous. It is not a limit anyone
// should reach honestly; it is the point at which Ferry stops waiting.
const HandshakeTimeout = 30 * time.Second

// maxPendingHandshakes caps how many connections may be mid-handshake at once.
// Past this, new connections are dropped rather than accumulating: a flood of
// half-open connections is the exact thing the timeout exists to survive, and
// dropping is cheaper than queueing.
const maxPendingHandshakes = 256

// NewListener wraps ln so every connection is served over TLS and must finish
// its handshake within timeout. A timeout of zero means HandshakeTimeout.
// Cancelling ctx, like closing the listener, aborts any handshake in flight.
//
// This exists because crypto/tls.NewListener does not do it. That listener
// defers the handshake to the first read or write and never sets a deadline of
// its own, so how long a silent client is humoured is decided by whichever
// protocol server happens to touch the connection first: five minutes for
// go-smtp, thirty seconds for go-imap. Apple's accountsd is such a client:
// while verifying an account it opens a second connection to the submission
// port and waits for a plaintext greeting that an implicit-TLS server will
// never send. Ferry waited for a ClientHello, accountsd waited for a 220, and
// the macOS profile installer sat behind both of them for five minutes a
// probe, which reads as a hang.
//
// The handshake runs off the accept loop, so one slow client cannot delay
// another, and Accept returns only connections that finished it. Accept also
// returns a *tls.Conn rather than a wrapper, because go-smtp and go-imap both
// recognise an encrypted connection by type-asserting for exactly that. Wrap
// it and they conclude the connection is in the clear and refuse to
// authenticate.
func NewListener(ctx context.Context, ln net.Listener, cfg *tls.Config, timeout time.Duration) net.Listener {
	if timeout <= 0 {
		timeout = HandshakeTimeout
	}
	ctx, cancel := context.WithCancel(ctx)
	l := &listener{
		inner:  ln,
		conns:  make(chan *tls.Conn),
		fail:   make(chan error, 1),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	go l.accept(ctx, cfg, timeout)
	return l
}

type listener struct {
	inner net.Listener

	conns chan *tls.Conn
	fail  chan error // the accept loop's terminal error, buffered so it never blocks
	done  chan struct{}

	// cancel aborts every in-flight handshake, so closing the listener does
	// not wait out their timeouts.
	cancel context.CancelFunc

	closeOnce sync.Once
}

var _ net.Listener = (*listener)(nil)

// accept runs the raw accept loop, handing each connection to its own
// handshake goroutine.
func (l *listener) accept(ctx context.Context, cfg *tls.Config, timeout time.Duration) {
	pending := make(chan struct{}, maxPendingHandshakes)
	for {
		raw, err := l.inner.Accept()
		if err != nil {
			select {
			case l.fail <- err:
			default:
			}
			return
		}

		select {
		case <-l.done:
			_ = raw.Close()
			return
		default:
		}

		select {
		case pending <- struct{}{}:
		default:
			// Already at the limit of half-open connections.
			_ = raw.Close()
			continue
		}

		go func() {
			defer func() { <-pending }()
			l.handshake(ctx, raw, cfg, timeout)
		}()
	}
}

// handshake completes the TLS handshake under a deadline and queues the
// connection, or closes it.
func (l *listener) handshake(ctx context.Context, raw net.Conn, cfg *tls.Config, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn := tls.Server(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return
	}
	// No deadline is set on the connection itself, deliberately: past the
	// handshake the protocol servers impose their own, and IMAP needs a long
	// one: a connection parked in IDLE is idle by design, for up to half an
	// hour, and must not be mistaken for a stalled one.

	select {
	case l.conns <- conn:
	case <-l.done:
		_ = conn.Close()
	}
}

// Accept returns the next connection whose handshake has completed.
func (l *listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case err := <-l.fail:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops the listener. Connections already returned by Accept are the
// caller's to close.
func (l *listener) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
		l.cancel()
	})
	return l.inner.Close()
}

// Addr returns the underlying listener's address.
func (l *listener) Addr() net.Addr { return l.inner.Addr() }
