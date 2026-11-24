package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// Limits of the Home Agent API.
const (
	MaxAgentBody     = 4 << 20 // whole request body
	MaxAgentHTML     = 2 << 20 // rendered_html
	maxAgentJobs     = 20
	agentPollSeconds = 60
	agentBlockBack   = 7 * time.Hour // keeps a challenged watch quiet for at least 6 hours even after the scheduler's -10% jitter
	agentTokenLimit  = 60            // requests per token per minute
	agentIPLimit     = 120           // requests per client address per minute
)

// agentAPI holds the state of the Home Agent endpoints.
type agentAPI struct {
	byToken *RateLimiter
	byIP    *RateLimiter
	sched   *Scheduler // used only for its result recording; it has no fetcher and never fetches
}

func newAgentAPI(store *Store, clock Clock, baseURL string) *agentAPI {
	return &agentAPI{
		byToken: NewRateLimiter(agentTokenLimit, time.Minute),
		byIP:    NewRateLimiter(agentIPLimit, time.Minute),
		sched:   NewScheduler(store, nil, clock, baseURL),
	}
}

func (s *Server) registerAgent(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent/jobs", s.withAgent(s.handleAgentJobs))
	mux.HandleFunc("POST /api/agent/result", s.withAgent(s.handleAgentResult))
	mux.HandleFunc("POST /account/agent/create", s.withSession(s.handleAgentCreate))
	mux.HandleFunc("POST /account/agent/{id}/revoke", s.withSession(s.handleAgentRevoke))
	mux.HandleFunc("POST /account/agent/{id}/rotate", s.withSession(s.handleAgentRotate))
}

type agentHandler func(w http.ResponseWriter, r *http.Request, uid int64)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// withAgent applies the rate limits first and only then checks the bearer
// token, so a flood of guesses is throttled before it costs a database lookup.
// Every authentication failure is an identical 404.
func (s *Server) withAgent(h agentHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := s.clock.Now()
		tok := ""
		if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "Bearer ") {
			tok = strings.TrimSpace(a[7:])
		}
		if len(tok) > 200 {
			tok = ""
		}
		if !s.agent.byIP.Allow(clientIP(r), now) || !s.agent.byToken.Allow(hashToken(tok), now) {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
			return
		}
		if !strings.HasPrefix(tok, AgentTokenPrefix) {
			http.NotFound(w, r)
			return
		}
		_, uid, err := s.store.AgentByHash(hashToken(tok))
		if err != nil {
			if !errors.Is(err, ErrInvalidToken) {
				slog.Error("agent auth", "err", RedactPII(err.Error()))
			}
			http.NotFound(w, r)
			return
		}
		h(w, r, uid)
	}
}

type agentJob struct {
	WatchID     int64  `json:"watch_id"`
	URL         string `json:"url"`
	Selector    string `json:"selector,omitempty"`
	Version     int64  `json:"version"`
	IntervalMin int    `json:"interval_min"`
	MinDelaySec int    `json:"min_delay_sec"`
	NextCheckAt int64  `json:"next_check_at"`
}

func (s *Server) handleAgentJobs(w http.ResponseWriter, r *http.Request, uid int64) {
	ws, err := s.store.AgentDueWatches(uid, maxAgentJobs)
	if err != nil {
		slog.Error("agent jobs", "err", RedactPII(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	jobs := make([]agentJob, 0, len(ws))
	for _, x := range ws {
		if !agentURLAllowed(x.URL) {
			continue
		}
		iv := x.IntervalMin
		if iv <= 0 {
			iv = DefaultIntervalMin
		}
		jobs = append(jobs, agentJob{WatchID: x.ID, URL: x.URL, Selector: x.Selector, Version: x.Version,
			IntervalMin: iv, MinDelaySec: iv * 60, NextCheckAt: x.NextCheckAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"server_time": s.clock.Now().Unix(), "poll_after_sec": agentPollSeconds, "jobs": jobs})
}

// agentURLAllowed reports whether a URL is one the agent may load: http or https only.
func agentURLAllowed(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}

type agentResult struct {
	WatchID      int64  `json:"watch_id"`
	Version      *int64 `json:"version"`
	Status       string `json:"status"`
	FailureClass string `json:"failure_class"`
	RenderedHTML string `json:"rendered_html"`
	FinalURL     string `json:"final_url"`
	HTTPStatus   int    `json:"http_status"`
	Timings      struct {
		LoadMS int64 `json:"load_ms"`
		IdleMS int64 `json:"idle_ms"`
	} `json:"timings"`
}

// agentFailureReasons maps failure_class to the text shown to the owner. The
// agent supplies only the class, never free text, so nothing it sends can put
// a URL or other detail into the dashboard.
var agentFailureReasons = map[string]string{
	"challenge": "blocked by site (bot challenge seen by your home agent)",
	"dns":       "your home agent could not look up the site's address",
	"connect":   "your home agent could not connect to the site",
	"tls":       "your home agent had a secure-connection problem with the site",
	"timeout":   "the page took too long to load on your home agent",
	"nav_error": "your home agent could not open the page",
	"too_large": "the rendered page was too large for your home agent to send",
	"redirect":  "the page redirected to a different site, so it was not read",
	"other":     "your home agent had a problem loading the page",
}

// sameSite reports whether two hosts are the same or share a registrable domain.
func sameSite(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ea, err1 := publicsuffix.EffectiveTLDPlusOne(a)
	eb, err2 := publicsuffix.EffectiveTLDPlusOne(b)
	return err1 == nil && err2 == nil && ea == eb
}

// finalURLOK checks the URL the browser ended on: http(s) only, on the watched
// host or the same site.
func finalURLOK(watchURL, final string) bool {
	if final == "" {
		return true // an agent that does not report it is judged on the page alone
	}
	u, err := url.Parse(final)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return sameSite(hostOf(watchURL), u.Hostname())
}

func (s *Server) handleAgentResult(w http.ResponseWriter, r *http.Request, uid int64) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxAgentBody)
	var req agentResult
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if len(req.RenderedHTML) > MaxAgentHTML {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "rendered_html too large"})
		return
	}
	switch req.Status {
	case "ok":
		if req.RenderedHTML == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "rendered_html required for ok"})
			return
		}
	case "blocked", "error":
		if req.RenderedHTML != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "rendered_html only for ok"})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be ok, blocked or error"})
		return
	}
	// Ownership: the watch must be the token owner's. A foreign or missing id is the same 404.
	wt, err := s.store.WatchByID(req.WatchID, uid)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if mode, err := s.store.FetchModeOf(wt.ID); err != nil || mode != FetchAgent {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "watch is not set to fetch from the home agent"})
		return
	}
	if req.Version != nil {
		wt.Version = *req.Version // the snapshot the agent was given; an edit since makes the result stale
	}
	s.agent.record(r.Context(), wt, &req)
	next := int64(0)
	if cur, err := s.store.WatchByID(wt.ID, uid); err == nil {
		next = cur.NextCheckAt
	}
	writeJSON(w, http.StatusOK, map[string]any{"recorded": true, "next_check_at": next})
}

// record runs the same extraction and recording as a server-side check.
func (a *agentAPI) record(ctx context.Context, w *Watch, req *agentResult) {
	var res *Result
	var fail *CheckFailure
	switch {
	case req.Status == "blocked":
		fail = &CheckFailure{Status: "blocked", Reason: agentFailureReasons["challenge"], RetryAfter: agentBlockBack}
	case req.Status == "error":
		reason, ok := agentFailureReasons[req.FailureClass]
		if !ok {
			reason = agentFailureReasons["other"]
		}
		fail = &CheckFailure{Status: "error", Reason: reason}
	case !finalURLOK(w.URL, req.FinalURL):
		fail = &CheckFailure{Status: "error", Reason: agentFailureReasons["redirect"]}
	default:
		code := req.HTTPStatus
		if code < 100 || code > 599 {
			code = 200
		}
		res, fail = CheckPage(&Page{URL: w.URL, Status: code, ContentType: "text/html", Header: http.Header{}, Body: []byte(req.RenderedHTML)}, w.Selector)
	}
	if fail != nil {
		a.sched.onFailure(ctx, w, fail)
		return
	}
	a.sched.onSuccess(ctx, w, res)
}

// ---- dashboard: token management ----

// agentTokenView is one row of the Home agent card.
type agentTokenView struct {
	ID       int64
	Label    string
	Created  string
	LastSeen string
}

func (s *Server) agentTokenViews(uid int64) []agentTokenView {
	ts, err := s.store.ListAgentTokens(uid)
	if err != nil {
		slog.Error("list agent tokens", "err", RedactPII(err.Error()))
		return nil
	}
	var out []agentTokenView
	for _, t := range ts {
		if t.RevokedAt != nil {
			continue
		}
		v := agentTokenView{ID: t.ID, Label: t.Label, Created: time.Unix(t.CreatedAt, 0).In(london).Format("2 Jan 2006, 15:04"), LastSeen: "never"}
		if t.LastSeenAt != nil {
			v.LastSeen = time.Unix(*t.LastSeenAt, 0).In(london).Format("2 Jan 2006, 15:04")
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) showAgentToken(w http.ResponseWriter, sess *Session, raw, label string) {
	d := pageData{Title: "Home agent token", UserID: sess.UserID, Phone: sess.Phone, CSRF: sess.CSRF, LoggedIn: true,
		Token: raw, Heading: label, Link: strings.TrimRight(s.cfg.BaseURL, "/")}
	s.render(w, http.StatusOK, "agent_token", d)
}

func (s *Server) handleAgentCreate(w http.ResponseWriter, r *http.Request, sess *Session) {
	label := strings.TrimSpace(r.PostFormValue("label"))
	if len([]rune(label)) > 40 {
		label = string([]rune(label)[:40])
	}
	if label == "" {
		label = "Home PC"
	}
	raw, hash := newAgentToken()
	if _, err := s.store.CreateAgentToken(sess.UserID, label, hash); err != nil {
		if errors.Is(err, ErrAgentTokenCap) {
			s.message(w, r, http.StatusConflict, "Limit reached", fmt.Sprintf("You can have at most %d agent tokens. Revoke one first.", MaxAgentTokensPerUser))
			return
		}
		slog.Error("create agent token", "err", RedactPII(err.Error()))
		s.message(w, r, 500, "Error", "Could not create the token.")
		return
	}
	s.showAgentToken(w, sess, raw, label)
}

func (s *Server) agentTokenID(r *http.Request) int64 {
	var id int64
	fmt.Sscanf(r.PathValue("id"), "%d", &id)
	return id
}

func (s *Server) handleAgentRevoke(w http.ResponseWriter, r *http.Request, sess *Session) {
	if err := s.store.RevokeAgentToken(sess.UserID, s.agentTokenID(r)); err != nil {
		s.message(w, r, http.StatusNotFound, "Not found", "That token does not exist.")
		return
	}
	http.Redirect(w, r, "/?m=agent_revoked", http.StatusSeeOther)
}

func (s *Server) handleAgentRotate(w http.ResponseWriter, r *http.Request, sess *Session) {
	id := s.agentTokenID(r)
	raw, hash := newAgentToken()
	if err := s.store.RotateAgentToken(sess.UserID, id, hash); err != nil {
		s.message(w, r, http.StatusNotFound, "Not found", "That token does not exist.")
		return
	}
	label := "Home PC"
	if ts, err := s.store.ListAgentTokens(sess.UserID); err == nil {
		for _, t := range ts {
			if t.ID == id {
				label = t.Label
			}
		}
	}
	s.showAgentToken(w, sess, raw, label)
}

func init() {
	flashes["agent_revoked"] = "Agent token revoked. Any agent using it stops working at once."
}

// agentSuggestedHosts are sites that block data-centre addresses and draw
// their prices with JavaScript. The form suggests the home agent for them; it
// never forces it.
var agentSuggestedHosts = []string{"jet2.com", "jet2holidays.com", "tui.co.uk", "easyjet.com"}

// AgentSuggested reports whether a host is, or is a subdomain of, a suggested site.
func AgentSuggested(host string) bool {
	host = strings.ToLower(host)
	for _, h := range agentSuggestedHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// applyFetchChoice reads the "Fetch from" select. An absent or unknown value
// leaves the watch alone, so forms that predate the field behave as before.
func (s *Server) applyFetchChoice(r *http.Request, uid, id int64) error {
	switch m := r.PostFormValue("fetch"); m {
	case FetchServer, FetchAgent:
		return s.store.SetFetchMode(uid, id, m)
	}
	return nil
}

// decorateAgentView adds the fetch mode, the offline state and the suggestion to a watch row.
func (s *Server) decorateAgentView(v *watchView, w *Watch, mode string) {
	if mode == "" {
		mode = FetchServer
	}
	v.Fetch, v.FetchAgent = mode, mode == FetchAgent
	v.SuggestAgent = mode == FetchServer && AgentSuggested(v.Host)
	if v.FetchAgent && !w.Paused && w.Failures < MaxFailures && AgentOffline(w, s.clock.Now().Unix()) &&
		(v.Status == "ok" || v.Status == "pending" || v.Status == "error") {
		v.Status, v.StatusTxt = "error", "Agent offline"
		v.Reason = "Your home agent has not reported for this page recently. Check that the PC is on and awake and the agent is running."
	}
}

// agentCard is the "Home agent" section of the dashboard.
type agentCard struct {
	Tokens   []agentTokenView
	Max      int
	CanAdd   bool
	LastSeen string
	Hosts    string
}

func (s *Server) agentCardFor(uid int64) *agentCard {
	c := &agentCard{Tokens: s.agentTokenViews(uid), Max: MaxAgentTokensPerUser, LastSeen: "never", Hosts: strings.Join(agentSuggestedHosts, ", ")}
	c.CanAdd = len(c.Tokens) < MaxAgentTokensPerUser
	if t, err := s.store.AgentLastSeen(uid); err == nil && t != nil {
		c.LastSeen = time.Unix(*t, 0).In(london).Format("2 Jan 2006, 15:04")
	}
	return c
}
