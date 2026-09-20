package resend_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LucasStbnr/ferry/internal/resend"
	"github.com/LucasStbnr/ferry/internal/testutil/fakeresend"
)

const key = "re_test_key"

func newClient(t *testing.T, baseURL string) *resend.Client {
	t.Helper()
	return resend.New(key, resend.WithBaseURL(baseURL), resend.WithRate(1000))
}

func TestListPaginates(t *testing.T) {
	api := fakeresend.New(key)
	defer api.Close()
	for i := 0; i < 25; i++ {
		m := &fakeresend.Mail{}
		m.Subject = "msg"
		api.AddReceived(m)
	}

	c := newClient(t, api.URL)
	ctx := context.Background()

	var (
		seen  []string
		after string
	)
	for {
		page, err := c.ListReceived(ctx, resend.ListParams{After: after, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			seen = append(seen, item.ID)
		}
		if !page.HasMore {
			break
		}
		after = page.Items[len(page.Items)-1].ID
	}
	if len(seen) != 25 {
		t.Fatalf("walked %d rows, want 25", len(seen))
	}
	unique := map[string]bool{}
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("id %s came back twice; the cursor is wrong", id)
		}
		unique[id] = true
	}
}

func TestRetriesRateLimitAndHonoursRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("ratelimit-remaining", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"name":"rate_limit_exceeded","message":"Too many requests"}`)
			return
		}
		_, _ = io.WriteString(w, `{"has_more":false,"data":[]}`)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	if _, err := c.ListReceived(context.Background(), resend.ListParams{}); err != nil {
		t.Fatalf("a rate limit should be retried transparently: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("made %d attempts, want 3 (two rejected, one accepted)", got)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"name":"rate_limit_exceeded"}`)
	}))
	defer srv.Close()

	c := resend.New(key, resend.WithBaseURL(srv.URL), resend.WithRate(1000), resend.WithMaxRetries(2))
	_, err := c.ListReceived(context.Background(), resend.ListParams{})
	if err == nil {
		t.Fatal("an endlessly rate-limited request should eventually fail")
	}
	if !resend.IsRateLimited(err) || !resend.IsTransient(err) {
		t.Fatalf("error = %v; a rate limit must stay classified as transient", err)
	}
}

func TestQuotaIsNotTreatedAsRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"name":"daily_quota_exceeded","message":"You have reached your daily limit"}`)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	_, err := c.Send(context.Background(), &resend.SendRequest{From: "a@b.test", To: []string{"c@d.test"}}, "")
	if err == nil {
		t.Fatal("a quota failure was reported as success")
	}
	// A quota arrives as 429 too, but retrying will not help until it resets,
	// so it must not be retried or classified as transient.
	if !resend.IsQuota(err) {
		t.Errorf("IsQuota = false for %v", err)
	}
	if resend.IsRateLimited(err) || resend.IsTransient(err) {
		t.Errorf("a quota failure was classified as retryable: %v", err)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(error) bool
		name   string
	}{
		{401, `{"name":"missing_api_key"}`, resend.IsAuth, "IsAuth"},
		{403, `{"name":"restricted_api_key"}`, resend.IsAuth, "IsAuth"},
		{404, `{"name":"not_found"}`, resend.IsNotFound, "IsNotFound"},
		{500, `{"name":"internal_server_error"}`, resend.IsTransient, "IsTransient"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		c := resend.New(key, resend.WithBaseURL(srv.URL), resend.WithRate(1000), resend.WithMaxRetries(0))
		_, err := c.GetReceived(context.Background(), "rcv-1")
		if err == nil {
			t.Errorf("status %d produced no error", tc.status)
		} else if !tc.check(err) {
			t.Errorf("status %d: %s returned false for %v", tc.status, tc.name, err)
		}
		srv.Close()
	}
}

func TestExpiredDownloadURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	_, err := c.Download(context.Background(), srv.URL+"/signed")
	if !errors.Is(err, resend.ErrExpired) {
		t.Fatalf("error = %v, want ErrExpired so sync can fall back to synthesising MIME", err)
	}
}

func TestSendUsesIdempotencyKey(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Idempotency-Key")
		_, _ = io.WriteString(w, `{"id":"snt-1"}`)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	id, err := c.Send(context.Background(),
		&resend.SendRequest{From: "a@b.test", To: []string{"c@d.test"}}, "ferry-abc")
	if err != nil {
		t.Fatal(err)
	}
	if id != "snt-1" {
		t.Errorf("id = %q", id)
	}
	if got != "ferry-abc" {
		t.Errorf("Idempotency-Key = %q; without it a retry sends the message twice", got)
	}
}

func TestSendWithoutIDIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	if _, err := c.Send(context.Background(), &resend.SendRequest{From: "a@b.test"}, ""); err == nil {
		t.Fatal("a response with no email id should not count as a successful send")
	}
}

func TestRateLimiterSpacesRequests(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		_, _ = io.WriteString(w, `{"has_more":false,"data":[]}`)
	}))
	defer srv.Close()

	// Four requests per second with a burst of two: six requests must take at
	// least a second, which is what keeps a backfill inside Resend's budget.
	c := resend.New(key, resend.WithBaseURL(srv.URL), resend.WithRate(4))
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 6; i++ {
		if _, err := c.ListReceived(ctx, resend.ListParams{}); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("six requests at 4/s took %v; the rate limiter is not holding", elapsed)
	}
}

func TestContextCancellationStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	c := newClient(t, srv.URL)
	if _, err := c.ListReceived(ctx, resend.ListParams{}); err == nil {
		t.Fatal("a cancelled request should return an error")
	}
}

func TestAddrsAcceptsBothShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Resend returns "to" as a bare string in some responses and an array
		// in others; both have to decode.
		_, _ = io.WriteString(w, `{"id":"rcv-1","to":"one@example.test","cc":["a@b.test","c@d.test"],"bcc":null}`)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	email, err := c.GetReceived(context.Background(), "rcv-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(email.To) != 1 || email.To[0] != "one@example.test" {
		t.Errorf("To = %v", email.To)
	}
	if len(email.Cc) != 2 {
		t.Errorf("Cc = %v", email.Cc)
	}
	if len(email.Bcc) != 0 {
		t.Errorf("Bcc = %v, want empty for null", email.Bcc)
	}
}

func TestDomainCanSend(t *testing.T) {
	cases := []struct {
		domain resend.Domain
		want   bool
	}{
		{resend.Domain{Status: "verified", Capabilities: resend.DomainCapabilities{Sending: "enabled"}}, true},
		{resend.Domain{Status: "verified", Capabilities: resend.DomainCapabilities{Sending: "disabled"}}, false},
		{resend.Domain{Status: "pending", Capabilities: resend.DomainCapabilities{Sending: "enabled"}}, false},
		{resend.Domain{Status: "verified"}, true}, // no capabilities reported
		{resend.Domain{Status: "not_started"}, false},
	}
	for _, tc := range cases {
		if got := tc.domain.CanSend(); got != tc.want {
			t.Errorf("CanSend(%+v) = %v, want %v", tc.domain, got, tc.want)
		}
	}
}

func TestAuthorizationHeaderIsSent(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"has_more":false,"data":[]}`)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	if _, err := c.ListSent(context.Background(), resend.ListParams{}); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer "+key {
		t.Fatalf("Authorization = %q", auth)
	}
}

func TestDownloadSendsNoCredentials(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "raw message")
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	rc, err := c.Download(context.Background(), srv.URL+"/signed")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "raw message" {
		t.Errorf("body = %q", body)
	}
	// Signed URLs are not on the API host, so the API key must not travel with
	// them.
	if auth != "" {
		t.Errorf("the API key was sent to a signed download URL: %q", auth)
	}
}

func TestBaseURLFromEnvironment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"has_more":false,"data":[]}`)
	}))
	defer srv.Close()

	t.Setenv(resend.EnvBaseURL, srv.URL+"/")
	c := resend.New(key, resend.WithRate(1000))
	if _, err := c.ListReceived(context.Background(), resend.ListParams{}); err != nil {
		t.Fatalf("the environment override was not used: %v", err)
	}

	// An explicit option still wins over the environment.
	var reached bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = io.WriteString(w, `{"has_more":false,"data":[]}`)
	}))
	defer other.Close()

	c2 := resend.New(key, resend.WithBaseURL(other.URL), resend.WithRate(1000))
	if _, err := c2.ListReceived(context.Background(), resend.ListParams{}); err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Error("WithBaseURL did not override the environment")
	}
}

func TestAPIErrorMessage(t *testing.T) {
	err := &resend.APIError{Status: 422, Name: "validation_error", Message: "from is required"}
	if got := err.Error(); !strings.Contains(got, "422") ||
		!strings.Contains(got, "validation_error") ||
		!strings.Contains(got, "from is required") {
		t.Fatalf("error message is missing detail: %q", got)
	}
}
