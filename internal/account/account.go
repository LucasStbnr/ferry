package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/LucasStbnr/ferry/internal/resend"
	"github.com/LucasStbnr/ferry/internal/secrets"
	"github.com/LucasStbnr/ferry/internal/store"
)

// ErrAuthFailed is returned for any failed login, whatever the reason.
var ErrAuthFailed = errors.New("account: authentication failed")

// Manager creates, lists and removes accounts, and authenticates sessions.
type Manager struct {
	DB      *store.DB
	Secrets secrets.Store
	Logger  *slog.Logger
	// NewClient builds an API client for a key. Tests replace it to point at
	// a fake server.
	NewClient func(apiKey string) *resend.Client
}

// NewManager creates a Manager with the production client factory.
func NewManager(db *store.DB, sec secrets.Store, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		DB:      db,
		Secrets: sec,
		Logger:  log,
		NewClient: func(apiKey string) *resend.Client {
			return resend.New(apiKey, resend.WithUserAgent("ferry"))
		},
	}
}

// Created describes a freshly added account. Password is the only time the app
// password exists in the clear.
type Created struct {
	Account  *store.Account
	Password string
	Domains  []string
}

// AddOptions configure Add.
type AddOptions struct {
	// Name identifies the account locally and is the IMAP/SMTP username.
	Name string
	// APIKey is the Resend API key. It is stored in the secret store, never in
	// the database, and never logged.
	APIKey string
	// Address is the From address Apple Mail should use. When empty it is
	// derived from the account's first verified domain.
	Address string
	// Password sets the app password instead of generating one.
	Password string
}

// Add registers an account. It verifies the API key against Resend first, so a
// typo fails immediately rather than at the first sync.
func (m *Manager) Add(ctx context.Context, opts AddOptions) (*Created, error) {
	if err := store.ValidateName(opts.Name); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("account: no API key given")
	}

	client := m.NewClient(opts.APIKey)
	domains, err := client.ListDomains(ctx)
	if err != nil {
		switch {
		case resend.IsAuth(err):
			return nil, fmt.Errorf("account: Resend rejected the API key; check that it is valid and has full access")
		default:
			return nil, fmt.Errorf("account: could not reach Resend: %w", err)
		}
	}

	var sending []string
	for _, d := range domains {
		if d.CanSend() {
			sending = append(sending, strings.ToLower(d.Name))
		}
	}

	address := strings.TrimSpace(opts.Address)
	if address == "" && len(sending) > 0 {
		address = "hello@" + sending[0]
	}

	password := opts.Password
	if password == "" {
		if password, err = GenerateAppPassword(); err != nil {
			return nil, err
		}
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}

	acct, err := m.DB.CreateAccount(ctx, opts.Name, address, hash)
	if err != nil {
		return nil, err
	}

	// From here on a failure must not leave a half-built account behind.
	rollback := func() {
		_ = m.DB.DeleteAccount(context.WithoutCancel(ctx), opts.Name)
		_ = m.Secrets.Delete(secrets.APIKey(opts.Name))
	}
	if err := m.Secrets.Set(secrets.APIKey(opts.Name), opts.APIKey); err != nil {
		rollback()
		return nil, fmt.Errorf("account: store API key: %w", err)
	}
	if err := m.DB.SetDomains(ctx, opts.Name, sending); err != nil {
		rollback()
		return nil, err
	}
	as := m.DB.Account(acct)
	if err := as.EnsureDefaultMailboxes(ctx); err != nil {
		rollback()
		return nil, err
	}

	acct.Domains = sending
	acct.Address = address
	m.Logger.Info("account added", "account", opts.Name, "domains", len(sending))
	return &Created{Account: acct, Password: password, Domains: sending}, nil
}

// Remove deletes an account, its mail and its secrets. The Resend account
// itself is untouched.
func (m *Manager) Remove(ctx context.Context, name string) error {
	acct, err := m.DB.AccountByName(ctx, name)
	if err != nil {
		return err
	}
	as := m.DB.Account(acct)

	if err := m.DB.DeleteAccount(ctx, name); err != nil {
		return err
	}
	// Blob files are only reachable through the rows just deleted, so this
	// reclaims every one of them.
	if _, _, err := as.GCBlobs(ctx); err != nil {
		m.Logger.Warn("could not remove stored messages", "account", name, "error", err)
	}
	if err := m.Secrets.Delete(secrets.APIKey(name)); err != nil {
		m.Logger.Warn("could not remove API key", "account", name, "error", err)
	}
	if err := m.Secrets.Delete(secrets.WebhookSecret(name)); err != nil {
		m.Logger.Warn("could not remove webhook secret", "account", name, "error", err)
	}
	m.Logger.Info("account removed", "account", name)
	return nil
}

// ResetPassword generates and stores a new app password.
func (m *Manager) ResetPassword(ctx context.Context, name, password string) (string, error) {
	if _, err := m.DB.AccountByName(ctx, name); err != nil {
		return "", err
	}
	var err error
	if password == "" {
		if password, err = GenerateAppPassword(); err != nil {
			return "", err
		}
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	if err := m.DB.SetPasswordHash(ctx, name, hash); err != nil {
		return "", err
	}
	m.Logger.Info("app password reset", "account", name)
	return password, nil
}

// APIKey returns an account's Resend key from the secret store.
func (m *Manager) APIKey(name string) (string, error) {
	key, err := m.Secrets.Get(secrets.APIKey(name))
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return "", fmt.Errorf("account: no API key stored for %q; run `ferry account add %s` again", name, name)
		}
		return "", err
	}
	return key, nil
}

// Client returns an API client for an account.
func (m *Manager) Client(name string) (*resend.Client, error) {
	key, err := m.APIKey(name)
	if err != nil {
		return nil, err
	}
	return m.NewClient(key), nil
}

// RefreshDomains re-reads the account's verified sending domains, which is how
// a domain verified after the account was added becomes usable for sending.
func (m *Manager) RefreshDomains(ctx context.Context, name string) ([]string, error) {
	client, err := m.Client(name)
	if err != nil {
		return nil, err
	}
	domains, err := client.ListDomains(ctx)
	if err != nil {
		return nil, err
	}
	var sending []string
	for _, d := range domains {
		if d.CanSend() {
			sending = append(sending, strings.ToLower(d.Name))
		}
	}
	if err := m.DB.SetDomains(ctx, name, sending); err != nil {
		return nil, err
	}
	return sending, nil
}

// Authenticate verifies an IMAP or SMTP login.
//
// It always performs a bcrypt comparison, even for an account that does not
// exist, so the time a failure takes does not reveal which names are real.
func (m *Manager) Authenticate(ctx context.Context, username, password string) (*store.Account, error) {
	acct, err := m.DB.AccountByName(ctx, username)
	if err != nil || acct.PasswordHash == "" {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, ErrAuthFailed
	}
	if !VerifyPassword(acct.PasswordHash, password) {
		return nil, ErrAuthFailed
	}
	return acct, nil
}

// Status summarises one account for `ferry status`.
type Status struct {
	Name       string
	Address    string
	Domains    []string
	Created    time.Time
	Counts     store.Counts
	Received   store.SyncState
	Sent       store.SyncState
	HasAPIKey  bool
	HasWebhook bool
}

// Status collects the state of every account.
func (m *Manager) Status(ctx context.Context) ([]Status, error) {
	accts, err := m.DB.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(accts))
	for i := range accts {
		a := &accts[i]
		as := m.DB.Account(a)
		st := Status{
			Name:    a.Name,
			Address: a.Address,
			Domains: a.Domains,
			Created: a.CreatedAt,
		}
		if st.Counts, err = as.Counts(ctx); err != nil {
			return nil, err
		}
		if st.Received, err = as.SyncState(ctx, store.KindReceived); err != nil {
			return nil, err
		}
		if st.Sent, err = as.SyncState(ctx, store.KindSent); err != nil {
			return nil, err
		}
		_, keyErr := m.Secrets.Get(secrets.APIKey(a.Name))
		st.HasAPIKey = keyErr == nil
		_, hookErr := m.Secrets.Get(secrets.WebhookSecret(a.Name))
		st.HasWebhook = hookErr == nil
		out = append(out, st)
	}
	return out, nil
}
