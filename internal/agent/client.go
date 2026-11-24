package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Job is one page the server wants read.
type Job struct {
	WatchID     int64  `json:"watch_id"`
	URL         string `json:"url"`
	Selector    string `json:"selector,omitempty"`
	Version     int64  `json:"version"`
	IntervalMin int    `json:"interval_min"`
	MinDelaySec int    `json:"min_delay_sec"`
	NextCheckAt int64  `json:"next_check_at"`
}

// JobList is the server's answer to a poll.
type JobList struct {
	ServerTime   int64 `json:"server_time"`
	PollAfterSec int   `json:"poll_after_sec"`
	Jobs         []Job `json:"jobs"`
}

// Timings is reported with each result.
type Timings struct {
	LoadMS int64 `json:"load_ms"`
	IdleMS int64 `json:"idle_ms"`
}

// Result is what the agent reports for a job.
type Result struct {
	WatchID      int64   `json:"watch_id"`
	Version      int64   `json:"version"`
	Status       string  `json:"status"` // ok, blocked, error
	FailureClass string  `json:"failure_class,omitempty"`
	RenderedHTML string  `json:"rendered_html,omitempty"`
	FinalURL     string  `json:"final_url,omitempty"`
	HTTPStatus   int     `json:"http_status,omitempty"`
	Timings      Timings `json:"timings"`
}

// Server is the Webwatcher server as the agent sees it.
type Server interface {
	Jobs(ctx context.Context) (*JobList, error)
	Submit(ctx context.Context, r *Result) error
}

// ErrRejected means the server did not accept the token (or this is not a Webwatcher server).
var ErrRejected = errors.New("the server rejected the agent token (revoked, replaced, or wrong server address)")

// RateLimitedError carries the server's Retry-After.
type RateLimitedError struct{ After time.Duration }

func (e *RateLimitedError) Error() string { return "rate limited by the server" }

// Client is the HTTP implementation of Server.
type Client struct {
	Base  *url.URL
	Token string
	HTTP  *http.Client
}

// NewClient validates the server address. Plain http is allowed only for a loopback host.
func NewClient(server, token string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(server), "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("server must be a URL such as https://watch.example.com")
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return nil, errors.New("the server address must be https (plain http is only accepted for localhost)")
	}
	if token == "" {
		return nil, errors.New("no agent token")
	}
	return &Client{Base: u, Token: token, HTTP: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func isLoopback(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	u := *c.Base
	u.Path = strings.TrimRight(u.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "webwatcher-agent")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, scrubErr(err, c.Token)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, scrubErr(err, c.Token)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrRejected
	case resp.StatusCode == http.StatusTooManyRequests:
		after := time.Minute
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
			after = time.Duration(n) * time.Second
		}
		return nil, &RateLimitedError{After: after}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("server answered HTTP %d", resp.StatusCode)
	}
	return data, nil
}

// scrubErr removes the token and the query string from a transport error.
func scrubErr(err error, token string) error {
	s := strings.ReplaceAll(err.Error(), token, "[token]")
	return errors.New(s)
}

// Jobs polls for due watches.
func (c *Client) Jobs(ctx context.Context) (*JobList, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/agent/jobs", nil)
	if err != nil {
		return nil, err
	}
	var l JobList
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, errors.New("the server's answer was not understood (is this a Webwatcher server?)")
	}
	return &l, nil
}

// Submit reports a result.
func (c *Client) Submit(ctx context.Context, r *Result) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, "/api/agent/result", b)
	return err
}
