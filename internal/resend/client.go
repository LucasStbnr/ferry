// Package resend is a small client for the parts of the Resend API that Ferry
// needs: reading received and sent mail, listing domains and sending.
package resend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// DefaultBaseURL is the production API endpoint.
const DefaultBaseURL = "https://api.resend.com"

// EnvBaseURL overrides the API endpoint for every client in the process. It
// exists so the test suite can point at a fake server and so a deployment can
// route through an egress proxy; an explicit WithBaseURL still wins.
const EnvBaseURL = "FERRY_RESEND_BASE_URL"

// Client talks to the Resend API. It is safe for concurrent use; every request
// goes through a shared token bucket so that a backfill can never exceed the
// per-team rate limit.
type Client struct {
	base    string
	key     string
	http    *http.Client
	limiter *rate.Limiter
	// maxRetries bounds automatic retries on 429 rate limiting.
	maxRetries int
	userAgent  string
	sleep      func(ctx context.Context, d time.Duration) error
}

// Option customises a Client.
type Option func(*Client)

// WithBaseURL points the client at another endpoint (tests, proxies).
func WithBaseURL(u string) Option { return func(c *Client) { c.base = u } }

// WithHTTPClient replaces the underlying HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithRate sets the sustained requests per second (default 4, half the team
// limit so that a website using the same account keeps its headroom).
func WithRate(rps float64) Option {
	return func(c *Client) { c.limiter = rate.NewLimiter(rate.Limit(rps), 2) }
}

// WithMaxRetries sets how many times a 429 is retried (default 5).
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option { return func(c *Client) { c.userAgent = ua } }

// New creates a client for the given API key.
func New(apiKey string, opts ...Option) *Client {
	base := DefaultBaseURL
	if u := strings.TrimSpace(os.Getenv(EnvBaseURL)); u != "" {
		base = strings.TrimSuffix(u, "/")
	}
	c := &Client{
		base:       base,
		key:        apiKey,
		http:       &http.Client{Timeout: 60 * time.Second},
		limiter:    rate.NewLimiter(4, 2),
		maxRetries: 5,
		userAgent:  "ferry",
		sleep:      sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type listResponse struct {
	HasMore bool      `json:"has_more"`
	Data    []Summary `json:"data"`
}

// ListDomains returns every domain of the account.
func (c *Client) ListDomains(ctx context.Context) ([]Domain, error) {
	var out []Domain
	after := ""
	for {
		q := url.Values{"limit": {"100"}}
		if after != "" {
			q.Set("after", after)
		}
		var resp struct {
			HasMore bool     `json:"has_more"`
			Data    []Domain `json:"data"`
		}
		if err := c.do(ctx, http.MethodGet, "/domains", q, nil, nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if !resp.HasMore || len(resp.Data) == 0 {
			return out, nil
		}
		after = resp.Data[len(resp.Data)-1].ID
	}
}

func (c *Client) list(ctx context.Context, path string, p ListParams) (*Page, error) {
	q := url.Values{}
	limit := p.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	q.Set("limit", strconv.Itoa(limit))
	if p.After != "" {
		q.Set("after", p.After)
	}
	var resp listResponse
	if err := c.do(ctx, http.MethodGet, path, q, nil, nil, &resp); err != nil {
		return nil, err
	}
	return &Page{HasMore: resp.HasMore, Items: resp.Data}, nil
}

// ListReceived lists inbound mail, newest first.
func (c *Client) ListReceived(ctx context.Context, p ListParams) (*Page, error) {
	return c.list(ctx, "/emails/receiving", p)
}

// GetReceived fetches one inbound message with body, headers and attachment
// metadata.
func (c *Client) GetReceived(ctx context.Context, id string) (*Email, error) {
	var e Email
	if err := c.do(ctx, http.MethodGet, "/emails/receiving/"+url.PathEscape(id), nil, nil, nil, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// GetReceivedAttachment returns an attachment with a fresh download URL.
func (c *Client) GetReceivedAttachment(ctx context.Context, emailID, attachmentID string) (*AttachmentInfo, error) {
	var a AttachmentInfo
	path := "/emails/receiving/" + url.PathEscape(emailID) + "/attachments/" + url.PathEscape(attachmentID)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// ListSent lists outbound mail, newest first.
func (c *Client) ListSent(ctx context.Context, p ListParams) (*Page, error) {
	return c.list(ctx, "/emails", p)
}

// GetSent fetches one outbound message.
func (c *Client) GetSent(ctx context.Context, id string) (*Email, error) {
	var e Email
	if err := c.do(ctx, http.MethodGet, "/emails/"+url.PathEscape(id), nil, nil, nil, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Send submits a message. idempotencyKey (if non-empty) makes retries safe for
// 24 hours. It returns the Resend email ID.
func (c *Client) Send(ctx context.Context, req *SendRequest, idempotencyKey string) (string, error) {
	hdr := http.Header{}
	if idempotencyKey != "" {
		hdr.Set("Idempotency-Key", idempotencyKey)
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/emails", nil, hdr, req, &resp); err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", errors.New("resend: send succeeded without an email id")
	}
	return resp.ID, nil
}

// Download streams a signed URL (raw message or attachment). Signed URLs are
// not part of the API host, so no credentials are attached. The caller must
// close the body. An expired URL yields ErrExpired.
func (c *Client) Download(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusForbidden, http.StatusNotFound, http.StatusGone:
			return nil, ErrExpired
		}
		return nil, &APIError{Status: resp.StatusCode, Message: "download failed"}
	}
	return resp.Body, nil
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, hdr http.Header, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	for attempt := 0; ; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, vs := range hdr {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_ = resp.Body.Close()
		if rerr != nil {
			return rerr
		}

		if resp.StatusCode/100 == 2 {
			if out == nil || len(data) == 0 {
				return nil
			}
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("resend: decode %s: %w", path, err)
			}
			return nil
		}

		apiErr := parseError(resp, data)
		if IsRateLimited(apiErr) && attempt < c.maxRetries {
			wait := apiErr.RetryAfter
			if wait <= 0 {
				wait = time.Duration(1<<attempt) * 500 * time.Millisecond
			}
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		return apiErr
	}
}

func parseError(resp *http.Response, data []byte) *APIError {
	e := &APIError{Status: resp.StatusCode}
	var body struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &body) == nil {
		e.Name, e.Message = body.Name, body.Message
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			e.RetryAfter = time.Duration(secs * float64(time.Second))
		}
	}
	return e
}
