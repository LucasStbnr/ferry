package resend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Time is a timestamp from the Resend API.
//
// The API is not consistent about its format: the inbound endpoints return
// RFC 3339 ("2026-09-14T19:33:51.212Z") while the sent-mail listing returns a
// Postgres-style timestamp with a space and a two-digit offset
// ("2026-09-14 19:33:51.212000+00"). encoding/json only understands the first,
// so a strict time.Time silently breaks half the API.
//
// Rather than guess one layout, every plausible one is tried in turn. An
// unparseable value yields the zero time rather than an error: a timestamp
// Ferry cannot read is not a reason to lose the message it belongs to.
type Time struct{ time.Time }

// layouts are tried in order. The offset-only forms come last because
// time.Parse accepts a superset with the earlier ones.
var layouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999Z0700",
	"2006-01-02 15:04:05.999999-07:00",
	"2006-01-02 15:04:05.999999-07",
	"2006-01-02 15:04:05.999999Z07:00",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *Time) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// A numeric timestamp is not something Resend sends today, but it
		// costs nothing to accept one.
		var unix int64
		if json.Unmarshal(b, &unix) == nil && unix > 0 {
			t.Time = time.Unix(unix, 0).UTC()
			return nil
		}
		return fmt.Errorf("resend: unexpected timestamp %s", b)
	}
	if s == "" {
		return nil
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time = parsed
			return nil
		}
	}
	// Deliberately not an error: see the type comment.
	return nil
}

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.Format(time.RFC3339Nano))
}
