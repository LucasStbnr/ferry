package account_test

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/LucasStbnr/ferry/internal/account"
	"github.com/LucasStbnr/ferry/internal/resend"
	"github.com/LucasStbnr/ferry/internal/secrets"
	"github.com/LucasStbnr/ferry/internal/store"
	"github.com/LucasStbnr/ferry/internal/testutil/fakeresend"
)

const apiKey = "re_test_key"

type fixture struct {
	api *fakeresend.Server
	db  *store.DB
	sec *secrets.Memory
	mgr *account.Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()

	api := fakeresend.New(apiKey)
	api.SetDomains(
		resend.Domain{ID: "d1", Name: "mysite.test", Status: "verified",
			Capabilities: resend.DomainCapabilities{Sending: "enabled"}},
		resend.Domain{ID: "d2", Name: "pending.test", Status: "pending",
			Capabilities: resend.DomainCapabilities{Sending: "enabled"}},
	)
	t.Cleanup(api.Close)

	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "ferry.db"), filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	sec := &secrets.Memory{}
	mgr := account.NewManager(db, sec, nil)
	mgr.NewClient = func(key string) *resend.Client {
		return resend.New(key, resend.WithBaseURL(api.URL), resend.WithRate(1000))
	}
	return &fixture{api: api, db: db, sec: sec, mgr: mgr}
}

func TestAddCreatesEverything(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	created, err := f.mgr.Add(ctx, account.AddOptions{Name: "mysite", APIKey: apiKey})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	// Only verified domains may be used for sending.
	if len(created.Domains) != 1 || created.Domains[0] != "mysite.test" {
		t.Fatalf("domains = %v, want only the verified one", created.Domains)
	}
	if created.Account.Address != "hello@mysite.test" {
		t.Errorf("address = %q", created.Account.Address)
	}

	// The key lives in the secret store, never in the database.
	stored, err := f.sec.Get(secrets.APIKey("mysite"))
	if err != nil || stored != apiKey {
		t.Fatalf("api key = %q, %v", stored, err)
	}
	acct, err := f.db.AccountByName(ctx, "mysite")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(acct.PasswordHash, created.Password) {
		t.Fatal("the app password is recoverable from the stored hash")
	}

	// The standard folders exist, so Mail has somewhere to file mail.
	boxes, err := f.db.Account(acct).Mailboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != len(store.DefaultMailboxes) {
		t.Fatalf("%d mailboxes created, want %d", len(boxes), len(store.DefaultMailboxes))
	}
}

func TestAddRejectsABadKeyWithoutLeavingState(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	if _, err := f.mgr.Add(ctx, account.AddOptions{Name: "mysite", APIKey: "re_wrong"}); err == nil {
		t.Fatal("an invalid API key was accepted")
	}
	if _, err := f.db.AccountByName(ctx, "mysite"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a failed add left an account row behind")
	}
	if _, err := f.sec.Get(secrets.APIKey("mysite")); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatal("a failed add left a secret behind")
	}
}

func TestAddRejectsDuplicateAndInvalidNames(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	if _, err := f.mgr.Add(ctx, account.AddOptions{Name: "mysite", APIKey: apiKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.Add(ctx, account.AddOptions{Name: "mysite", APIKey: apiKey}); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate add gave %v", err)
	}
	for _, bad := range []string{"", "Upper", "has space", "../escape", strings.Repeat("a", 64)} {
		if _, err := f.mgr.Add(ctx, account.AddOptions{Name: bad, APIKey: apiKey}); err == nil {
			t.Errorf("account name %q was accepted", bad)
		}
	}
}

func TestRemoveClearsMailAndSecrets(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	created, err := f.mgr.Add(ctx, account.AddOptions{Name: "mysite", APIKey: apiKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.sec.Set(secrets.WebhookSecret("mysite"), "whsec_x"); err != nil {
		t.Fatal(err)
	}

	as := f.db.Account(created.Account)
	inbox, err := as.Mailbox(ctx, store.Inbox)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: kept\r\n\r\nbody\r\n")
	if _, err := as.Append(ctx, inbox.ID, &store.NewMessage{Raw: raw}); err != nil {
		t.Fatal(err)
	}
	hash := store.HashBytes(raw)

	if err := f.mgr.Remove(ctx, "mysite"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AccountByName(ctx, "mysite"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the account row survived removal")
	}
	if _, err := f.sec.Get(secrets.APIKey("mysite")); !errors.Is(err, secrets.ErrNotFound) {
		t.Error("the API key survived removal")
	}
	if _, err := f.sec.Get(secrets.WebhookSecret("mysite")); !errors.Is(err, secrets.ErrNotFound) {
		t.Error("the webhook secret survived removal")
	}
	if as.HasBlob(hash) {
		t.Error("the stored message file survived removal")
	}
}

func TestAuthenticate(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	created, err := f.mgr.Add(ctx, account.AddOptions{Name: "mysite", APIKey: apiKey})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.mgr.Authenticate(ctx, "mysite", created.Password); err != nil {
		t.Fatalf("the generated password was rejected: %v", err)
	}
	if _, err := f.mgr.Authenticate(ctx, "mysite", "wrong"); !errors.Is(err, account.ErrAuthFailed) {
		t.Errorf("wrong password gave %v", err)
	}
	if _, err := f.mgr.Authenticate(ctx, "nosuch", created.Password); !errors.Is(err, account.ErrAuthFailed) {
		t.Errorf("unknown account gave %v", err)
	}
}

// TestAuthenticateDoesNotLeakAccountNames checks that failing against an
// unknown account costs roughly as much as failing against a real one. If it
// returned early, the response time alone would enumerate account names.
func TestAuthenticateDoesNotLeakAccountNames(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.mgr.Add(ctx, account.AddOptions{Name: "mysite", APIKey: apiKey}); err != nil {
		t.Fatal(err)
	}

	measure := func(name string) time.Duration {
		start := time.Now()
		for i := 0; i < 3; i++ {
			_, _ = f.mgr.Authenticate(ctx, name, "definitely-not-the-password")
		}
		return time.Since(start) / 3
	}
	real, unknown := measure("mysite"), measure("does-not-exist")

	ratio := float64(unknown) / float64(real)
	if ratio < 0.2 || ratio > 5 {
		t.Errorf("an unknown account took %v against %v for a real one (ratio %.2f); "+
			"the difference is large enough to enumerate account names", unknown, real, ratio)
	}
}

func TestResetPassword(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	created, err := f.mgr.Add(ctx, account.AddOptions{Name: "mysite", APIKey: apiKey})
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.mgr.ResetPassword(ctx, "mysite", "")
	if err != nil {
		t.Fatal(err)
	}
	if next == created.Password {
		t.Fatal("reset returned the same password")
	}
	if _, err := f.mgr.Authenticate(ctx, "mysite", created.Password); err == nil {
		t.Error("the old password still works after a reset")
	}
	if _, err := f.mgr.Authenticate(ctx, "mysite", next); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
	if _, err := f.mgr.ResetPassword(ctx, "nosuch", ""); err == nil {
		t.Error("resetting a missing account succeeded")
	}
}

func TestRefreshDomainsPicksUpNewVerification(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.mgr.Add(ctx, account.AddOptions{Name: "mysite", APIKey: apiKey}); err != nil {
		t.Fatal(err)
	}

	// The second domain finishes verifying in Resend.
	f.api.VerifyDomain("pending.test")

	domains, err := f.mgr.RefreshDomains(ctx, "mysite")
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) != 2 {
		t.Fatalf("domains = %v, want both", domains)
	}
	acct, err := f.db.AccountByName(ctx, "mysite")
	if err != nil {
		t.Fatal(err)
	}
	if len(acct.Domains) != 2 {
		t.Fatalf("stored domains = %v", acct.Domains)
	}
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.mgr.Add(ctx, account.AddOptions{Name: "mysite", APIKey: apiKey}); err != nil {
		t.Fatal(err)
	}

	statuses, err := f.mgr.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("%d statuses", len(statuses))
	}
	s := statuses[0]
	if !s.HasAPIKey {
		t.Error("HasAPIKey = false although a key was stored")
	}
	if s.HasWebhook {
		t.Error("HasWebhook = true although no webhook secret was stored")
	}
	if s.Counts.Mailboxes != int64(len(store.DefaultMailboxes)) {
		t.Errorf("mailboxes = %d", s.Counts.Mailboxes)
	}
}

var appPasswordRE = regexp.MustCompile(`^[a-z2-9]{4}(-[a-z2-9]{4}){4}$`)

func TestGeneratedPasswordsAreStrongAndReadable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p, err := account.GenerateAppPassword()
		if err != nil {
			t.Fatal(err)
		}
		if !appPasswordRE.MatchString(p) {
			t.Fatalf("password %q does not match the expected shape", p)
		}
		// Characters that are easy to confuse when typed by hand are excluded.
		if strings.ContainsAny(p, "l10") {
			t.Fatalf("password %q contains an easily misread character", p)
		}
		if seen[p] {
			t.Fatalf("password %q was generated twice in 200 draws", p)
		}
		seen[p] = true
	}
}

func TestPasswordHashing(t *testing.T) {
	hash, err := account.HashPassword("abcd-efgh-ijkl-mnop-qrst")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, "abcd") {
		t.Fatal("the hash contains the password")
	}
	if !account.VerifyPassword(hash, "abcd-efgh-ijkl-mnop-qrst") {
		t.Error("the correct password did not verify")
	}
	if account.VerifyPassword(hash, "abcd-efgh-ijkl-mnop-qrsu") {
		t.Error("a wrong password verified")
	}
	if _, err := account.HashPassword(""); err == nil {
		t.Error("an empty password was hashed")
	}

	// Two hashes of the same password differ, so the salt is doing its job.
	other, _ := account.HashPassword("abcd-efgh-ijkl-mnop-qrst")
	if other == hash {
		t.Error("two hashes of the same password are identical")
	}
}

func TestAPIKeyErrorIsActionable(t *testing.T) {
	f := newFixture(t)
	_, err := f.mgr.APIKey("nosuch")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ferry account add") {
		t.Errorf("error = %q; it should say how to fix the problem", err)
	}
}
