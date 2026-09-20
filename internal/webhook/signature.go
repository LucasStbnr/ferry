// Package webhook receives Resend's event callbacks, so new mail appears in
// Apple Mail the moment it arrives instead of at the next poll, and so bounces
// become something the user can actually see.
//
// Every request must carry a valid Svix signature. An unsigned receiver would
// let anyone who found the URL inject messages into the user's Inbox, so
// Ferry has no option to turn verification off.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Tolerance is how far a request's timestamp may be from now. It bounds how
// long a captured request stays replayable.
const Tolerance = 5 * time.Minute

// Errors returned by Verify.
var (
	ErrNoSignature   = errors.New("webhook: request has no signature")
	ErrBadSignature  = errors.New("webhook: signature does not match")
	ErrStaleRequest  = errors.New("webhook: timestamp is outside the tolerance window")
	ErrMalformedHead = errors.New("webhook: malformed signature headers")
)

// Verifier checks Svix-style signatures, the scheme Resend uses for webhooks.
//
// The signed string is "<id>.<timestamp>.<body>" and the signature is the
// base64 HMAC-SHA256 of it under the endpoint secret.
type Verifier struct {
	key []byte
	now func() time.Time
}

// NewVerifier parses an endpoint secret. Resend presents it as "whsec_" plus
// base64; the prefix is optional here so a pasted value works either way.
func NewVerifier(secret string) (*Verifier, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, errors.New("webhook: empty signing secret")
	}
	raw := strings.TrimPrefix(secret, "whsec_")
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// Some deployments configure a raw secret rather than base64.
		key = []byte(raw)
	}
	if len(key) == 0 {
		return nil, errors.New("webhook: signing secret decodes to nothing")
	}
	return &Verifier{key: key, now: time.Now}, nil
}

// Verify checks a request's headers against its body.
func (v *Verifier) Verify(h http.Header, body []byte) error {
	id := firstHeader(h, "Svix-Id", "Webhook-Id")
	timestamp := firstHeader(h, "Svix-Timestamp", "Webhook-Timestamp")
	signatures := firstHeader(h, "Svix-Signature", "Webhook-Signature")
	if id == "" || timestamp == "" || signatures == "" {
		return ErrNoSignature
	}

	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: timestamp %q", ErrMalformedHead, timestamp)
	}
	drift := v.now().Sub(time.Unix(secs, 0))
	if drift < 0 {
		drift = -drift
	}
	if drift > Tolerance {
		return ErrStaleRequest
	}

	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	want := mac.Sum(nil)

	// The header carries space-separated "v1,<base64>" pairs; any one of them
	// matching is enough, which is what lets a secret be rotated.
	matched := false
	for _, entry := range strings.Fields(signatures) {
		_, encoded, found := strings.Cut(entry, ",")
		if !found {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		// Compare every candidate rather than returning early, so the time
		// taken does not depend on which one matched.
		if subtle.ConstantTimeCompare(got, want) == 1 {
			matched = true
		}
	}
	if !matched {
		return ErrBadSignature
	}
	return nil
}

func firstHeader(h http.Header, names ...string) string {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// Sign produces the headers for a request body. It exists so tests exercise
// the real verification path rather than a stub.
func (v *Verifier) Sign(id string, ts time.Time, body []byte) http.Header {
	timestamp := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	h := http.Header{}
	h.Set("Svix-Id", id)
	h.Set("Svix-Timestamp", timestamp)
	h.Set("Svix-Signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return h
}
