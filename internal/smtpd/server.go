package smtpd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/LucasStbnr/ferry/internal/mailmime"
	"github.com/LucasStbnr/ferry/internal/resend"
	"github.com/LucasStbnr/ferry/internal/store"
)

// DefaultMaxMessageBytes caps a submission. Resend's own attachment limit is
// 40 MB; the headroom covers base64 expansion and headers.
const DefaultMaxMessageBytes int64 = 60 << 20

// Authenticator verifies an account's app password, exactly as the IMAP
// server does.
type Authenticator interface {
	Authenticate(ctx context.Context, username, password string) (*store.Account, error)
}

// ClientFactory returns an API client for an account.
type ClientFactory interface {
	Client(account string) (*resend.Client, error)
}

// Options configure the submission server.
type Options struct {
	// Addr is the listen address, e.g. 127.0.0.1:1465.
	Addr string
	// TLSConfig is required: Ferry uses implicit TLS, so credentials never
	// cross the socket in the clear.
	TLSConfig *tls.Config
	DB        *store.DB
	Auth      Authenticator
	Clients   ClientFactory
	// MaxMessageBytes caps one submission; zero means DefaultMaxMessageBytes.
	MaxMessageBytes int64
	// Hostname is announced in the SMTP greeting.
	Hostname string
	Logger   *slog.Logger
	// OnSent is called after a successful send, so the IMAP server can wake
	// clients idling on Sent.
	OnSent func(account string, mailboxID int64)
	// DebugWriter, if set, receives the raw protocol stream, credentials and
	// all. It is only ever enabled deliberately.
	DebugWriter io.Writer
}

// Server is the SMTP submission server.
type Server struct {
	opts Options
	log  *slog.Logger
	smtp *smtp.Server
}

// New creates a submission server.
func New(opts Options) (*Server, error) {
	switch {
	case opts.DB == nil:
		return nil, errors.New("smtpd: no database")
	case opts.Auth == nil:
		return nil, errors.New("smtpd: no authenticator")
	case opts.Clients == nil:
		return nil, errors.New("smtpd: no client factory")
	case opts.TLSConfig == nil:
		return nil, errors.New("smtpd: TLS is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if opts.Hostname == "" {
		opts.Hostname = "ferry.localhost"
	}

	s := &Server{opts: opts, log: opts.Logger}

	srv := smtp.NewServer(backend{s})
	srv.Addr = opts.Addr
	srv.Domain = opts.Hostname
	srv.TLSConfig = opts.TLSConfig
	srv.MaxRecipients = MaxRecipients
	srv.MaxMessageBytes = opts.MaxMessageBytes
	// A send waits on the Resend API, so the write timeout must outlast a slow
	// upload of a large attachment.
	srv.ReadTimeout = 5 * time.Minute
	srv.WriteTimeout = 5 * time.Minute
	srv.EnableSMTPUTF8 = true
	srv.AllowInsecureAuth = false
	srv.ErrorLog = slogAdapter{opts.Logger}
	srv.Debug = opts.DebugWriter

	s.smtp = srv
	return s, nil
}

// Listen binds the configured address with implicit TLS.
func (s *Server) Listen(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("smtpd: listen on %s: %w", s.opts.Addr, err)
	}
	return tls.NewListener(ln, s.opts.TLSConfig), nil
}

// Serve accepts connections on an already-TLS-wrapped listener.
func (s *Server) Serve(ln net.Listener) error { return s.smtp.Serve(ln) }

// Close stops the server.
func (s *Server) Close() error { return s.smtp.Close() }

type backend struct{ s *Server }

func (b backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	remote := ""
	if c != nil && c.Conn() != nil {
		remote = c.Conn().RemoteAddr().String()
	}
	return &session{
		srv: b.s,
		log: b.s.log.With("remote", remote),
	}, nil
}

// session is one SMTP connection, bound to one account after AUTH.
type session struct {
	srv *Server
	log *slog.Logger

	account *store.Account
	from    string
	rcpts   []string
}

var (
	_ smtp.Session     = (*session)(nil)
	_ smtp.AuthSession = (*session)(nil)
)

// AuthMechanisms advertises what clients may use. Both send the password in
// the clear, which is why the connection is TLS from the first byte.
func (s *session) AuthMechanisms() []string {
	return []string{sasl.Plain, sasl.Login}
}

// Auth wires a SASL server to the account authenticator.
func (s *session) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			if identity != "" && identity != username {
				return smtp.ErrAuthFailed
			}
			return s.authenticate(username, password)
		}), nil
	case sasl.Login:
		return newLoginServer(s.authenticate), nil
	}
	return nil, smtp.ErrAuthUnsupported
}

func (s *session) authenticate(username, password string) error {
	acct, err := s.srv.opts.Auth.Authenticate(context.Background(), username, password)
	if err != nil {
		s.log.Warn("smtp login failed", "username", username)
		return smtp.ErrAuthFailed
	}
	s.account = acct
	s.log = s.log.With("account", acct.Name)
	s.log.Info("smtp login")
	return nil
}

// Reset discards the message in progress.
func (s *session) Reset() {
	s.from = ""
	s.rcpts = nil
}

// Logout ends the session.
func (s *session) Logout() error { return nil }

// Mail records the envelope sender and checks it against the account's
// verified domains before anything is uploaded.
func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	if s.account == nil {
		return smtp.ErrAuthRequired
	}
	if err := checkFromDomain(from, s.account.Domains); err != nil {
		return smtpError(err)
	}
	s.from = from
	s.rcpts = nil
	return nil
}

// Rcpt records one envelope recipient.
func (s *session) Rcpt(to string, opts *smtp.RcptOptions) error {
	if s.account == nil {
		return smtp.ErrAuthRequired
	}
	if len(s.rcpts) >= MaxRecipients {
		return &smtp.SMTPError{
			Code: 452, EnhancedCode: smtp.EnhancedCode{4, 5, 3},
			Message: fmt.Sprintf("Resend accepts at most %d recipients per message", MaxRecipients),
		}
	}
	s.rcpts = append(s.rcpts, to)
	return nil
}

// Data reads the message, sends it through Resend and files a copy in Sent.
// It returns only once Resend has accepted or rejected the message.
func (s *session) Data(r io.Reader) error {
	if s.account == nil {
		return smtp.ErrAuthRequired
	}
	if len(s.rcpts) == 0 {
		return &smtp.SMTPError{
			Code: 554, EnhancedCode: smtp.EnhancedCode{5, 5, 1},
			Message: "No recipients given",
		}
	}

	ctx := context.Background()

	sub, err := mailmime.ParseSubmission(r, s.srv.opts.MaxMessageBytes)
	if err != nil {
		return &smtp.SMTPError{
			Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0},
			Message: "Could not read the message: " + err.Error(),
		}
	}
	if sub.From == "" {
		sub.From = s.from
	}
	// The header From is what the recipient sees, so it is checked too: the
	// envelope alone could be a domain the account owns while the visible
	// address is not.
	if err := checkFromDomain(sub.From, s.account.Domains); err != nil {
		return smtpError(err)
	}

	req, err := buildSendRequest(sub, s.rcpts)
	if err != nil {
		return smtpError(err)
	}

	client, err := s.srv.opts.Clients.Client(s.account.Name)
	if err != nil {
		return smtpError(&SendError{
			Permanent: true, Code: 550, Enhanced: [3]int{5, 7, 0},
			Message: "Ferry has no Resend API key for this account: " + err.Error(),
		})
	}

	resendID, err := send(ctx, client, req, idempotencyKey(sub))
	if err != nil {
		s.log.Warn("send failed", "subject", sub.Subject, "error", err)
		return smtpError(err)
	}
	s.log.Info("sent message", "resend_id", resendID, "recipients", len(s.rcpts))

	// The copy in Sent is the message the user actually composed, stored
	// verbatim rather than rebuilt from what Resend accepted.
	if err := s.fileInSent(ctx, sub, resendID); err != nil {
		// The message is sent; failing the transaction now would make Mail
		// send it again. Report it loudly instead.
		s.log.Error("message sent but could not be filed in Sent",
			"resend_id", resendID, "error", err)
	}
	return nil
}

func (s *session) fileInSent(ctx context.Context, sub *mailmime.Submission, resendID string) error {
	as := s.srv.opts.DB.Account(s.account)
	mbox, err := as.Mailbox(ctx, store.Sent)
	if err != nil {
		return err
	}
	idx := mailmime.Parse(sub.Raw)
	messageID := sub.MessageID
	if messageID == "" {
		messageID = idx.MessageID
	}
	if _, err := as.Append(ctx, mbox.ID, &store.NewMessage{
		Raw:          sub.Raw,
		ResendID:     resendID,
		MessageID:    messageID,
		InternalDate: time.Now(),
		SentDate:     idx.Date,
		Subject:      sub.Subject,
		From:         sub.From,
		To:           strings.Join(sub.To, ", "),
		SearchText:   idx.Text,
		Flags:        []string{`\Seen`},
	}); err != nil {
		return err
	}
	if s.srv.opts.OnSent != nil {
		s.srv.opts.OnSent(s.account.Name, mbox.ID)
	}
	return nil
}

// smtpError converts a SendError into the reply the client sees. Mail shows
// this text verbatim, so it is written for a person, not a log.
func smtpError(err error) error {
	var se *SendError
	if !errors.As(err, &se) {
		return &smtp.SMTPError{
			Code: 451, EnhancedCode: smtp.EnhancedCode{4, 0, 0},
			Message: err.Error(),
		}
	}
	return &smtp.SMTPError{
		Code:         se.Code,
		EnhancedCode: smtp.EnhancedCode(se.Enhanced),
		Message:      se.Message,
	}
}

// slogAdapter implements smtp.Logger on top of slog.
type slogAdapter struct{ log *slog.Logger }

func (a slogAdapter) Printf(format string, args ...any) {
	a.log.Warn(fmt.Sprintf(format, args...), "component", "smtp")
}

func (a slogAdapter) Println(args ...any) {
	a.log.Warn(strings.TrimRight(fmt.Sprintln(args...), "\n"), "component", "smtp")
}
