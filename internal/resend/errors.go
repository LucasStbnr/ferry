package resend

import (
	"errors"
	"fmt"
	"time"
)

// APIError is a non-2xx response from Resend.
type APIError struct {
	Status     int
	Name       string
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("resend: %d", e.Status)
	if e.Name != "" {
		msg += " " + e.Name
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// IsRateLimited reports a transient 429 (requests per second), as opposed to
// a quota exhaustion.
func IsRateLimited(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == 429 && !IsQuota(err)
}

// IsQuota reports an exhausted daily/monthly quota, including the 403 that
// blocks retrieving inbound content once the quota is used up.
func IsQuota(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Name {
	case "daily_quota_exceeded", "monthly_quota_exceeded", "email_above_quota":
		return true
	}
	return false
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == 404
}

// IsAuth reports a credential or permission problem.
func IsAuth(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == 401 || (ae.Status == 403 && !IsQuota(err)))
}

// IsTransient reports errors worth retrying later (rate limit, 5xx).
func IsTransient(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return IsRateLimited(err) || ae.Status >= 500
	}
	return false
}

// ErrExpired is returned when a signed download URL is no longer valid.
var ErrExpired = errors.New("resend: download URL expired")
