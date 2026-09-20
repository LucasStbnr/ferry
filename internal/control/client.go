package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"time"
)

// ErrNoDaemon means nothing is listening on the control socket.
var ErrNoDaemon = errors.New("control: no ferry daemon is running")

// Client talks to a running daemon.
type Client struct {
	http *http.Client
}

// Dial prepares a client for the socket at path. It does not connect yet;
// the first request reports whether a daemon is there.
func Dial(path string) *Client {
	return &Client{
		http: &http.Client{
			Timeout: 10 * time.Minute, // a forced backfill can take a while
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", path)
				},
			},
		},
	}
}

// Available reports whether a daemon is listening on the socket. A socket
// file left behind by a crashed daemon is not "available": nothing answers it.
func Available(ctx context.Context, path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Status fetches the daemon's status.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var out Status
	if err := c.do(ctx, http.MethodGet, "/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Sync asks the daemon to sync now.
func (c *Client) Sync(ctx context.Context, req SyncRequest) (*SyncResult, error) {
	var out SyncResult
	if err := c.do(ctx, http.MethodPost, "/sync", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reload asks the daemon to pick up account changes.
func (c *Client) Reload(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/reload", nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(payload)
	}
	// The host is ignored: the transport always dials the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://ferry"+path, rd)
	if err != nil {
		return err
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) || errors.Is(err, fs.ErrNotExist) {
			return ErrNoDaemon
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("control: daemon returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
