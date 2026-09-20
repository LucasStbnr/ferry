package resend_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/LucasStbnr/ferry/internal/resend"
)

// Resend does not use one timestamp format across its API. These are the
// shapes seen in real responses; the Postgres-style one comes from the sent
// listing and broke sent sync entirely until Time stopped being a time.Time.
func TestTimeAcceptsEveryFormatResendSends(t *testing.T) {
	want := time.Date(2026, 9, 14, 19, 33, 51, 212000000, time.UTC)

	cases := map[string]time.Time{
		// The format that actually broke: GET /emails.
		`"2026-09-14 19:33:51.212000+00"`:    want,
		`"2026-09-14T19:33:51.212Z"`:         want,
		`"2026-09-14T19:33:51.212000Z"`:      want,
		`"2026-09-14 19:33:51.212000+00:00"`: want,
		`"2026-09-14T19:33:51Z"`:             time.Date(2026, 9, 14, 19, 33, 51, 0, time.UTC),
		`"2026-09-14 19:33:51"`:              time.Date(2026, 9, 14, 19, 33, 51, 0, time.UTC),
		`"2026-09-14"`:                       time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
	}
	for input, expected := range cases {
		var got resend.Time
		if err := json.Unmarshal([]byte(input), &got); err != nil {
			t.Errorf("%s: %v", input, err)
			continue
		}
		if !got.UTC().Equal(expected) {
			t.Errorf("%s parsed as %v, want %v", input, got.UTC(), expected)
		}
	}
}

func TestTimeToleratesMissingAndUnparseableValues(t *testing.T) {
	// A timestamp Ferry cannot read must not cost us the message it belongs
	// to, so these yield the zero time rather than an error.
	for _, input := range []string{`null`, `""`, `"not a date at all"`, `"2026-13-45 99:99:99"`} {
		var got resend.Time
		if err := json.Unmarshal([]byte(input), &got); err != nil {
			t.Errorf("%s returned an error: %v", input, err)
		}
		if !got.IsZero() {
			t.Errorf("%s parsed as %v, want the zero time", input, got.Time)
		}
	}
}

// TestSummaryDecodesRealSentPayload is the regression test for the bug: the
// whole sent listing failed to decode, so sent mail never synced and the
// backfill never finished.
func TestSummaryDecodesRealSentPayload(t *testing.T) {
	const payload = `{
		"has_more": false,
		"data": [{
			"id": "3cf3e6d4-6d20-4444-9f7e-a0b77ef036bb",
			"from": "hello@mysite.test",
			"to": "someone@example.test",
			"subject": "Congrats on launching",
			"created_at": "2026-09-14 19:33:51.212000+00"
		}]
	}`
	var resp struct {
		HasMore bool             `json:"has_more"`
		Data    []resend.Summary `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		t.Fatalf("a real sent-listing response must decode: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("%d rows decoded", len(resp.Data))
	}
	if resp.Data[0].CreatedAt.IsZero() {
		t.Error("created_at was dropped")
	}
	if len(resp.Data[0].To) != 1 || resp.Data[0].To[0] != "someone@example.test" {
		t.Errorf("to = %v", resp.Data[0].To)
	}
}

func TestTimeRoundTrips(t *testing.T) {
	original := resend.Time{Time: time.Date(2026, 9, 14, 19, 33, 51, 0, time.UTC)}
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var back resend.Time
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Equal(original.Time) {
		t.Fatalf("round trip gave %v, want %v", back.Time, original.Time)
	}
	if b, _ := json.Marshal(resend.Time{}); string(b) != "null" {
		t.Errorf("zero time marshalled as %s, want null", b)
	}
}
