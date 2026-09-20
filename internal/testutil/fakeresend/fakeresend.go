// Package fakeresend is an in-memory imitation of the Resend HTTP API, used by
// tests so that no test ever touches the network.
package fakeresend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LucasStbnr/ferry/internal/resend"
)

// Mail is a message held by the fake, either received or sent.
type Mail struct {
	resend.Email
	// RawMIME, if set, is served through raw.download_url.
	RawMIME []byte
	// Attachment bodies by attachment ID.
	AttachmentData map[string][]byte
}

// Server is the fake API.
//
// The server starts handling requests as soon as New returns, so everything
// a test can change is guarded by the same mutex the handler reads under and
// is reached through a setter. Exported mutable fields would look convenient
// and race with any request already in flight.
type Server struct {
	*httptest.Server
	Key string

	mu       sync.Mutex
	received []*Mail // newest first
	sent     []*Mail // newest first
	domains  []resend.Domain
	requests []string // "METHOD /path?query"
	sendReqs []resend.SendRequest
	nextID   int

	// failNext429 makes the next N API requests return 429 with Retry-After: 0.
	failNext429 int
	// sendStatus/sendName, when set, make POST /emails fail.
	sendStatus int
	sendName   string
	// expireDownloads makes signed URLs answer 403, as they do once the
	// signature has aged out.
	expireDownloads bool
	// detailStatus/detailName make GET of a single received email fail.
	detailStatus int
	detailName   string
	idem         map[string]string
}

// SetDomains replaces the domain list.
func (s *Server) SetDomains(domains ...resend.Domain) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.domains = domains
}

// VerifyDomain marks a domain as verified, as finishing DNS setup would.
func (s *Server) VerifyDomain(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.domains {
		if s.domains[i].Name == name {
			s.domains[i].Status = "verified"
		}
	}
}

// FailNext429 makes the next n API requests answer 429 with Retry-After: 0.
func (s *Server) FailNext429(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext429 = n
}

// FailSend makes POST /emails answer with the given status and error name.
// A zero status restores normal behaviour.
func (s *Server) FailSend(status int, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendStatus, s.sendName = status, name
}

// FailDetail makes fetching a single received email fail. A zero status
// restores normal behaviour.
func (s *Server) FailDetail(status int, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detailStatus, s.detailName = status, name
}

// ExpireDownloads makes every signed download URL answer 403, which is what
// Resend's raw-message and attachment links do once they age out.
func (s *Server) ExpireDownloads(expired bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireDownloads = expired
}

// Attach adds an attachment with its content to a message already added.
func (s *Server) Attach(m *Mail, a resend.Attached, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.Attachments = append(m.Attachments, a)
	if m.AttachmentData == nil {
		m.AttachmentData = map[string][]byte{}
	}
	m.AttachmentData[a.ID] = data
}

// New starts a fake server; Close it when done.
func New(key string) *Server {
	s := &Server{Key: key, idem: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *Server) newID(prefix string) string {
	s.nextID++
	return fmt.Sprintf("%s-%04d", prefix, s.nextID)
}

// AddReceived appends a received message that is *newer* than all existing
// ones and returns it. ID and CreatedAt are filled in when empty.
func (s *Server) AddReceived(m *Mail) *Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		m.ID = s.newID("rcv")
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = resend.Time{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(len(s.received)) * time.Minute)}
	}
	s.received = append([]*Mail{m}, s.received...)
	return m
}

// AddSent appends a sent message that is newer than all existing ones.
func (s *Server) AddSent(m *Mail) *Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		m.ID = s.newID("snt")
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = resend.Time{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(len(s.sent)) * time.Minute)}
	}
	s.sent = append([]*Mail{m}, s.sent...)
	return m
}

// RequestCount counts recorded requests whose line contains substr.
func (s *Server) RequestCount(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if strings.Contains(r, substr) {
			n++
		}
	}
	return n
}

// Sends returns the recorded send requests.
func (s *Server) Sends() []resend.SendRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]resend.SendRequest(nil), s.sendReqs...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, name, msg string) {
	writeJSON(w, status, map[string]any{"statusCode": status, "name": name, "message": msg})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())

	path := r.URL.Path
	if strings.HasPrefix(path, "/download/") {
		s.download(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.Key {
		writeErr(w, 401, "missing_api_key", "Missing or invalid API key")
		return
	}
	if s.failNext429 > 0 {
		s.failNext429--
		w.Header().Set("Retry-After", "0")
		writeErr(w, 429, "rate_limit_exceeded", "Too many requests")
		return
	}

	switch {
	case r.Method == http.MethodGet && path == "/domains":
		writeJSON(w, 200, map[string]any{"object": "list", "has_more": false, "data": s.domains})
	case r.Method == http.MethodGet && path == "/emails/receiving":
		s.list(w, r, s.received)
	case r.Method == http.MethodGet && path == "/emails":
		s.list(w, r, s.sent)
	case r.Method == http.MethodPost && path == "/emails":
		s.send(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/emails/receiving/"):
		s.get(w, strings.TrimPrefix(path, "/emails/receiving/"), s.received, true)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/emails/"):
		s.get(w, strings.TrimPrefix(path, "/emails/"), s.sent, false)
	default:
		writeErr(w, 404, "not_found", "Route not found")
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, all []*Mail) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	start := 0
	if after := r.URL.Query().Get("after"); after != "" {
		start = len(all)
		for i, m := range all {
			if m.ID == after {
				start = i + 1
				break
			}
		}
	}
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	rows := make([]resend.Summary, 0, end-start)
	for _, m := range all[start:end] {
		rows = append(rows, resend.Summary{
			ID: m.ID, From: m.From, To: m.To, Cc: m.Cc, Bcc: m.Bcc, Subject: m.Subject,
			MessageID: m.MessageID, CreatedAt: m.CreatedAt, Attachments: m.Attachments,
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "has_more": end < len(all), "data": rows})
}

func (s *Server) get(w http.ResponseWriter, rest string, all []*Mail, received bool) {
	parts := strings.Split(rest, "/")
	var m *Mail
	for _, c := range all {
		if c.ID == parts[0] {
			m = c
		}
	}
	if m == nil {
		writeErr(w, 404, "not_found", "Email not found")
		return
	}
	if len(parts) == 3 && parts[1] == "attachments" {
		for _, a := range m.Attachments {
			if a.ID == parts[2] {
				writeJSON(w, 200, map[string]any{
					"object": "attachment", "id": a.ID, "filename": a.Filename, "size": a.Size,
					"content_type": a.ContentType, "content_disposition": a.Disposition, "content_id": a.ContentID,
					"download_url": s.URL + "/download/att/" + m.ID + "/" + a.ID,
					"expires_at":   time.Now().Add(time.Hour),
				})
				return
			}
		}
		writeErr(w, 404, "not_found", "Attachment not found")
		return
	}
	if received && s.detailStatus != 0 {
		writeErr(w, s.detailStatus, s.detailName, "injected failure")
		return
	}
	e := m.Email
	if received && m.RawMIME != nil {
		e.Raw = &resend.Raw{DownloadURL: s.URL + "/download/raw/" + m.ID, ExpiresAt: resend.Time{Time: time.Now().Add(time.Hour)}}
	} else {
		e.Raw = nil
	}
	writeJSON(w, 200, e)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	if s.expireDownloads {
		http.Error(w, "expired", http.StatusForbidden)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/download/"), "/")
	for _, m := range s.received {
		switch {
		case parts[0] == "raw" && len(parts) == 2 && m.ID == parts[1]:
			_, _ = w.Write(m.RawMIME)
			return
		case parts[0] == "att" && len(parts) == 3 && m.ID == parts[1]:
			if d, ok := m.AttachmentData[parts[2]]; ok {
				_, _ = w.Write(d)
				return
			}
		}
	}
	http.NotFound(w, r)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	if s.sendStatus != 0 {
		writeErr(w, s.sendStatus, s.sendName, "injected send failure")
		return
	}
	var req resend.SendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 422, "invalid_parameter", err.Error())
		return
	}
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		if id, ok := s.idem[key]; ok {
			writeJSON(w, 200, map[string]string{"id": id})
			return
		}
	}
	s.sendReqs = append(s.sendReqs, req)
	id := s.newID("snt")
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		s.idem[key] = id
	}
	writeJSON(w, 200, map[string]string{"id": id})
}
