package imapd

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/LucasStbnr/ferry/internal/store"
	"github.com/LucasStbnr/ferry/internal/tlsutil"
)

// DefaultAppendLimit caps a single APPEND. Resend will not accept an outgoing
// message anywhere near this size; the limit exists so a runaway client cannot
// fill the disk.
const DefaultAppendLimit int64 = 64 << 20

// Authenticator verifies an account's app password. It is a separate interface
// so the password hashing lives in one place and the IMAP and SMTP servers
// cannot drift apart on it.
type Authenticator interface {
	// Authenticate returns the account for a username and password, or an
	// error. Implementations must take the same time whether or not the
	// account exists.
	Authenticate(ctx context.Context, username, password string) (*store.Account, error)
}

// Options configure the IMAP server.
type Options struct {
	// Addr is the listen address, e.g. 127.0.0.1:1993.
	Addr string
	// TLSConfig is required: Ferry never serves IMAP in the clear, even on
	// loopback, so a password is never on the wire unprotected.
	TLSConfig *tls.Config
	// DB and Auth provide storage and authentication.
	DB   *store.DB
	Auth Authenticator
	// AppendLimit caps APPEND; zero means DefaultAppendLimit.
	AppendLimit int64
	Logger      *slog.Logger
	// DebugWriter, if set, receives the raw protocol stream. It contains
	// credentials, so it is only ever enabled deliberately.
	DebugWriter io.Writer
}

// Server serves IMAP for every account in the database.
type Server struct {
	opts Options
	log  *slog.Logger
	imap *imapserver.Server

	mu    sync.Mutex
	users map[string]*user // by account name
}

// New creates an IMAP server.
func New(opts Options) (*Server, error) {
	if opts.DB == nil {
		return nil, errors.New("imapd: no database")
	}
	if opts.Auth == nil {
		return nil, errors.New("imapd: no authenticator")
	}
	if opts.TLSConfig == nil {
		return nil, errors.New("imapd: TLS is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	s := &Server{opts: opts, log: opts.Logger, users: map[string]*user{}}

	caps := imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapIMAP4rev2: {},
		// IMAP4rev1 clients (which most still are, Apple Mail included) need
		// these announced explicitly even though IMAP4rev2 subsumes them.
		imap.CapNamespace:    {},
		imap.CapUIDPlus:      {},
		imap.CapESearch:      {},
		imap.CapSearchRes:    {},
		imap.CapListExtended: {},
		imap.CapListStatus:   {},
		imap.CapMove:         {},
		imap.CapStatusSize:   {},
		imap.CapLiteralMinus: {},
		imap.CapIdle:         {},
		imap.CapUnselect:     {},
		imap.CapSpecialUse:   {},
		imap.CapBinary:       {},
	}

	s.imap = imapserver.New(&imapserver.Options{
		NewSession:  s.newSession,
		Caps:        caps,
		Logger:      slogAdapter{opts.Logger},
		TLSConfig:   opts.TLSConfig,
		DebugWriter: opts.DebugWriter,
	})
	return s, nil
}

func (s *Server) appendLimit() int64 {
	if s.opts.AppendLimit > 0 {
		return s.opts.AppendLimit
	}
	return DefaultAppendLimit
}

func (s *Server) newSession(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{
		server: s,
		log:    s.log.With("remote", remoteAddr(conn)),
		ctx:    ctx,
		cancel: cancel,
	}
	return sess, &imapserver.GreetingData{PreAuth: false}, nil
}

func remoteAddr(conn *imapserver.Conn) string {
	if conn == nil {
		return ""
	}
	if a := conn.NetConn(); a != nil {
		return a.RemoteAddr().String()
	}
	return ""
}

// authenticate verifies credentials and returns the shared per-account state.
func (s *Server) authenticate(ctx context.Context, username, password string) (*user, error) {
	acct, err := s.opts.Auth.Authenticate(ctx, username, password)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	u, ok := s.users[acct.Name]
	if !ok {
		u = newUser(s.opts.DB.Account(acct))
		s.users[acct.Name] = u
	}
	s.mu.Unlock()

	if err := u.load(ctx); err != nil {
		return nil, fmt.Errorf("imapd: load mailboxes for %s: %w", acct.Name, err)
	}
	return u, nil
}

// MailboxChanged implements mailsync.Notifier: it reloads a mailbox so that
// clients idling on it are told about new mail immediately.
func (s *Server) MailboxChanged(account string, mailboxID int64) {
	s.mu.Lock()
	u := s.users[account]
	s.mu.Unlock()
	if u == nil {
		// Nobody is connected for this account; the next SELECT reads fresh
		// state from the store anyway.
		return
	}
	if err := u.refresh(context.Background(), mailboxID); err != nil {
		s.log.Warn("could not refresh mailbox after sync", "account", account, "mailbox_id", mailboxID, "error", err)
	}
}

// ForgetAccount drops cached state for an account that was removed.
func (s *Server) ForgetAccount(name string) {
	s.mu.Lock()
	delete(s.users, name)
	s.mu.Unlock()
}

// Serve accepts connections on ln. The listener must already be wrapped in
// TLS: Ferry uses implicit TLS on 993-style ports rather than STARTTLS, so
// there is no cleartext phase to strip.
func (s *Server) Serve(ln net.Listener) error {
	return s.imap.Serve(ln)
}

// Listen binds the configured address with implicit TLS.
func (s *Server) Listen(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("imapd: listen on %s: %w", s.opts.Addr, err)
	}
	return tlsutil.NewListener(ctx, ln, s.opts.TLSConfig, tlsutil.HandshakeTimeout), nil
}

// Close stops the server and drops every connection.
func (s *Server) Close() error { return s.imap.Close() }

// slogAdapter lets go-imap log through slog instead of the standard logger.
type slogAdapter struct{ log *slog.Logger }

func (a slogAdapter) Printf(format string, args ...any) {
	a.log.Warn(fmt.Sprintf(format, args...), "component", "imap")
}

// ConstantTimeCompare compares two secrets without leaking their contents
// through timing. It is exported so smtpd uses exactly the same comparison.
func ConstantTimeCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
