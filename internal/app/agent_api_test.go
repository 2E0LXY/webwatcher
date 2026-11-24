package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---- helpers ----

func agentTok(t *testing.T, e *tenv, phone string) (raw string, uid int64) {
	t.Helper()
	uid, err := e.st.EnsureUser(phone, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, h := newAgentToken()
	if _, err := e.st.CreateAgentToken(uid, "Home PC", h); err != nil {
		t.Fatal(err)
	}
	return raw, uid
}

func agentWatch(t *testing.T, e *tenv, uid int64, u string, target *int64, sel string) int64 {
	t.Helper()
	id, err := e.st.AddWatch(Watch{UserID: uid, URL: u, Label: "L", Selector: sel, TargetPence: target, IntervalMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetFetchMode(uid, id, FetchAgent); err != nil {
		t.Fatal(err)
	}
	return id
}

func agentDo(e *tenv, tok, method, path string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.RemoteAddr = "203.0.113.50:4000"
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func agentPostResult(e *tenv, tok string, v any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(v)
	return agentDo(e, tok, "POST", "/api/agent/result", b)
}

func jsonldPage(price string) string {
	return `<html><head><script type="application/ld+json">{"@type":"Product","offers":{"@type":"Offer","priceCurrency":"GBP","price":"` + price + `"}}</script></head><body>rendered</body></html>`
}

const selectorPage = `<html><body><div class="price-box"><span class="amount">£1,299 per person</span></div></body></html>`

type jobsResp struct {
	Jobs []agentJob `json:"jobs"`
}

func getJobs(t *testing.T, e *tenv, tok string) []agentJob {
	t.Helper()
	w := agentDo(e, tok, "GET", "/api/agent/jobs", nil)
	if w.Code != 200 {
		t.Fatalf("jobs: %d", w.Code)
	}
	var r jobsResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r.Jobs
}

// ---- tokens ----

func TestAgentTokenHashRotateRevoke(t *testing.T) {
	e := newTenv(t, nil)
	raw, uid := agentTok(t, e, "+447700900123")
	if !strings.HasPrefix(raw, AgentTokenPrefix) || len(raw) < 40 {
		t.Fatalf("token %q", raw)
	}
	var h string
	e.st.db.QueryRow(`SELECT token_hash FROM agent_tokens WHERE user_id=?`, uid).Scan(&h)
	if h == raw || h != hashToken(raw) || len(h) != 64 {
		t.Fatalf("hash %q", h)
	}
	var n int
	e.st.db.QueryRow(`SELECT COUNT(*) FROM agent_tokens WHERE token_hash=? OR label=?`, raw, raw).Scan(&n)
	if n != 0 {
		t.Fatal("raw token stored")
	}
	id, got, err := e.st.AgentByHash(hashToken(raw))
	if err != nil || got != uid {
		t.Fatal(err)
	}
	ts, _ := e.st.ListAgentTokens(uid)
	if len(ts) != 1 || ts[0].LastSeenAt == nil {
		t.Fatalf("last seen not recorded: %+v", ts)
	}
	// rotate: the old secret dies at once, the new one works, same id
	raw2, h2 := newAgentToken()
	if err := e.st.RotateAgentToken(uid, id, h2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.st.AgentByHash(hashToken(raw)); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("old token still valid after rotate")
	}
	if tid, _, err := e.st.AgentByHash(hashToken(raw2)); err != nil || tid != id {
		t.Fatal(tid, err)
	}
	// another account cannot rotate or revoke it
	other, _ := e.st.EnsureUser("+447700900124", false)
	if err := e.st.RevokeAgentToken(other, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign revoke: %v", err)
	}
	if err := e.st.RotateAgentToken(other, id, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign rotate: %v", err)
	}
	// revoke
	if err := e.st.RevokeAgentToken(uid, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.st.AgentByHash(hashToken(raw2)); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("revoked token valid")
	}
	if err := e.st.RevokeAgentToken(uid, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("second revoke should fail")
	}
}

func TestAgentTokenCapAndDisabledAccount(t *testing.T) {
	e := newTenv(t, nil)
	uid, _ := e.st.EnsureUser("+447700900123", false)
	var toks []string
	for i := 0; i < MaxAgentTokensPerUser; i++ {
		raw, h := newAgentToken()
		toks = append(toks, raw)
		if _, err := e.st.CreateAgentToken(uid, "x", h); err != nil {
			t.Fatal(err)
		}
	}
	_, h := newAgentToken()
	if _, err := e.st.CreateAgentToken(uid, "x", h); !errors.Is(err, ErrAgentTokenCap) {
		t.Fatalf("cap: %v", err)
	}
	// a revoked token frees a slot
	ts, _ := e.st.ListAgentTokens(uid)
	e.st.RevokeAgentToken(uid, ts[0].ID)
	if _, err := e.st.CreateAgentToken(uid, "x", h); err != nil {
		t.Fatal(err)
	}
	// a disabled account's tokens stop working
	e.st.db.Exec(`UPDATE users SET disabled_at=1 WHERE id=?`, uid)
	if w := agentDo(e, toks[1], "GET", "/api/agent/jobs", nil); w.Code != 404 {
		t.Fatalf("disabled account: %d", w.Code)
	}
}

func TestAgentTokenDashboardFlow(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700 900123")
	// creating needs the CSRF token
	if w := b.do("POST", "/account/agent/create", url.Values{"label": {"PC"}}); w.Code != 403 {
		t.Fatalf("no csrf: %d", w.Code)
	}
	w := b.do("POST", "/account/agent/create", url.Values{"csrf": {csrf}, "label": {"Study PC"}})
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	m := regexpAgentTok.FindString(w.Body.String())
	if m == "" {
		t.Fatalf("token not shown: %s", w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("token page cacheable")
	}
	// the dashboard never shows the secret again, only the name and last seen
	d := b.do("GET", "/", nil).Body.String()
	if strings.Contains(d, m) || !strings.Contains(d, "Study PC") || !strings.Contains(d, "never") {
		t.Fatalf("dashboard: token leaked or card missing")
	}
	if len(getJobs(t, e, m)) != 0 {
		t.Fatal("unexpected jobs")
	}
	d = b.do("GET", "/", nil).Body.String()
	if strings.Contains(d, "Last seen</th>") && !strings.Contains(d, "Home agent") {
		t.Fatal("card missing")
	}
	// cap
	for i := 0; i < MaxAgentTokensPerUser-1; i++ {
		b.do("POST", "/account/agent/create", url.Values{"csrf": {csrf}})
	}
	if w := b.do("POST", "/account/agent/create", url.Values{"csrf": {csrf}}); w.Code != http.StatusConflict {
		t.Fatalf("cap: %d", w.Code)
	}
	// another user cannot revoke or rotate it
	b2 := e.browser()
	csrf2 := b2.login("07700 900999")
	ts, _ := e.st.ListAgentTokens(1)
	path := fmt.Sprintf("/account/agent/%d/revoke", ts[0].ID)
	if w := b2.do("POST", path, url.Values{"csrf": {csrf2}}); w.Code != 404 {
		t.Fatalf("foreign revoke: %d", w.Code)
	}
	if w := b2.do("POST", fmt.Sprintf("/account/agent/%d/rotate", ts[0].ID), url.Values{"csrf": {csrf2}}); w.Code != 404 {
		t.Fatalf("foreign rotate: %d", w.Code)
	}
	if w := agentDo(e, m, "GET", "/api/agent/jobs", nil); w.Code != 200 {
		t.Fatalf("token should still work: %d", w.Code)
	}
	// owner rotates then revokes
	w = b.do("POST", fmt.Sprintf("/account/agent/%d/rotate", ts[0].ID), url.Values{"csrf": {csrf}})
	m2 := regexpAgentTok.FindString(w.Body.String())
	if w.Code != 200 || m2 == "" || m2 == m {
		t.Fatalf("rotate: %d", w.Code)
	}
	if agentDo(e, m, "GET", "/api/agent/jobs", nil).Code != 404 || agentDo(e, m2, "GET", "/api/agent/jobs", nil).Code != 200 {
		t.Fatal("rotate did not swap the secret")
	}
	if w := b.do("POST", path, url.Values{"csrf": {csrf}}); w.Code != 303 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if agentDo(e, m2, "GET", "/api/agent/jobs", nil).Code != 404 {
		t.Fatal("revoked token works")
	}
}

// ---- auth and limits ----

func TestAgentAPIAuth(t *testing.T) {
	e := newTenv(t, nil)
	raw, _ := agentTok(t, e, "+447700900123")
	for name, tok := range map[string]string{"none": "", "garbage": "nonsense", "wrong prefix": "wwa_" + strings.Repeat("a", 43), "session-like": strings.Repeat("b", 43), "huge": strings.Repeat("a", 5000)} {
		w := agentDo(e, tok, "GET", "/api/agent/jobs", nil)
		if w.Code != 404 {
			t.Errorf("%s: %d", name, w.Code)
		}
		if w.Body.Len() == 0 || strings.Contains(w.Body.String(), "token") {
			t.Errorf("%s: body leaks %q", name, w.Body.String())
		}
	}
	if w := agentDo(e, raw, "GET", "/api/agent/jobs", nil); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "json") {
		t.Fatalf("good token: %d", w.Code)
	}
	// a personal login link or session is not an agent token
	pl, _ := plink(t, e, "+447700900123")
	if agentDo(e, pl, "GET", "/api/agent/jobs", nil).Code != 404 {
		t.Fatal("personal link accepted")
	}
	// wrong method
	if w := agentDo(e, raw, "POST", "/api/agent/jobs", nil); w.Code == 200 {
		t.Fatal("POST jobs accepted")
	}
}

func TestAgentAPIRateLimitBeforeAuth(t *testing.T) {
	e := newTenv(t, nil)
	raw, _ := agentTok(t, e, "+447700900123")
	// Guessing with one bad token: 404 until the per-token limiter trips, then 429.
	var codes []int
	for i := 0; i < agentTokenLimit+3; i++ {
		codes = append(codes, agentDo(e, "wwa_bad", "GET", "/api/agent/jobs", nil).Code)
	}
	if codes[0] != 404 || codes[agentTokenLimit-1] != 404 || codes[agentTokenLimit] != 429 {
		t.Fatalf("codes %v", codes)
	}
	// the genuine token has its own budget
	if agentDo(e, raw, "GET", "/api/agent/jobs", nil).Code != 200 {
		t.Fatal("good token throttled by another token's guesses")
	}
	// per token: a valid token is limited too
	var last int
	for i := 0; i < agentTokenLimit+2; i++ {
		last = agentDo(e, raw, "GET", "/api/agent/jobs", nil).Code
	}
	if last != 429 {
		t.Fatalf("valid token not limited: %d", last)
	}
	// per address: many distinct bad tokens
	e2 := newTenv(t, nil)
	var c429 bool
	for i := 0; i < agentIPLimit+5; i++ {
		if agentDo(e2, fmt.Sprintf("wwa_%d", i), "GET", "/api/agent/jobs", nil).Code == 429 {
			c429 = true
		}
	}
	if !c429 {
		t.Fatal("address limiter never tripped")
	}
}

func TestAgentResultSizeCaps(t *testing.T) {
	e := newTenv(t, nil)
	raw, uid := agentTok(t, e, "+447700900123")
	id := agentWatch(t, e, uid, "https://www.jet2.com/p", nil, "")
	// whole body over 4 MB
	big := bytes.Repeat([]byte("a"), MaxAgentBody+10)
	if w := agentDo(e, raw, "POST", "/api/agent/result", big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("4MB cap: %d", w.Code)
	}
	// html over 2 MB but body under 4 MB
	w := agentPostResult(e, raw, map[string]any{"watch_id": id, "status": "ok", "rendered_html": strings.Repeat("a", MaxAgentHTML+1)})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("2MB cap: %d", w.Code)
	}
	// exactly the cap is accepted as far as size goes (no price, so a noprice failure is recorded)
	w = agentPostResult(e, raw, map[string]any{"watch_id": id, "status": "ok", "rendered_html": "<html>" + strings.Repeat("a", MaxAgentHTML-100) + "</html>"})
	if w.Code != 200 {
		t.Fatalf("at cap: %d %s", w.Code, w.Body.String())
	}
	// html is refused with non-ok statuses; unknown status rejected
	if w := agentPostResult(e, raw, map[string]any{"watch_id": id, "status": "error", "failure_class": "dns", "rendered_html": "<p>x</p>"}); w.Code != 400 {
		t.Fatalf("html with error: %d", w.Code)
	}
	if w := agentPostResult(e, raw, map[string]any{"watch_id": id, "status": "great"}); w.Code != 400 {
		t.Fatalf("bad status: %d", w.Code)
	}
	if w := agentPostResult(e, raw, map[string]any{"watch_id": id, "status": "ok"}); w.Code != 400 {
		t.Fatalf("ok without html: %d", w.Code)
	}
	if w := agentDo(e, raw, "POST", "/api/agent/result", []byte("{not json")); w.Code != 400 {
		t.Fatalf("bad json: %d", w.Code)
	}
}

// ---- isolation ----

func TestAgentCrossAccountIsolation(t *testing.T) {
	e := newTenv(t, nil)
	tokA, uidA := agentTok(t, e, "+447700900123")
	tokB, uidB := agentTok(t, e, "+447700900124")
	idA := agentWatch(t, e, uidA, "https://www.jet2.com/a", nil, "")
	idB := agentWatch(t, e, uidB, "https://www.easyjet.com/b", nil, "")
	// also a server-mode watch of A: never offered to the agent
	e.st.AddWatch(Watch{UserID: uidA, URL: "https://shop.example.com/s", IntervalMin: 60})

	ja, jb := getJobs(t, e, tokA), getJobs(t, e, tokB)
	if len(ja) != 1 || ja[0].WatchID != idA || ja[0].URL != "https://www.jet2.com/a" {
		t.Fatalf("A jobs %+v", ja)
	}
	if len(jb) != 1 || jb[0].WatchID != idB {
		t.Fatalf("B jobs %+v", jb)
	}
	// A submits a result for B's watch: indistinguishable from a missing watch, and nothing is recorded
	w := agentPostResult(e, tokA, map[string]any{"watch_id": idB, "status": "ok", "rendered_html": jsonldPage("100.00")})
	if w.Code != 404 {
		t.Fatalf("cross-account result: %d", w.Code)
	}
	if w2 := agentPostResult(e, tokA, map[string]any{"watch_id": 99999, "status": "ok", "rendered_html": jsonldPage("100.00")}); w2.Code != 404 || w2.Body.String() != w.Body.String() {
		t.Fatalf("missing differs from foreign: %d %q vs %q", w2.Code, w2.Body.String(), w.Body.String())
	}
	h, _ := e.st.History(uidB, idB, 10)
	wb, _ := e.st.WatchByID(idB, uidB)
	if len(h) != 0 || wb.Status != "pending" {
		t.Fatalf("B's watch was modified: %+v %d", wb, len(h))
	}
	// A's own result works, and does not touch B
	if w := agentPostResult(e, tokA, map[string]any{"watch_id": idA, "status": "ok", "rendered_html": jsonldPage("100.00")}); w.Code != 200 {
		t.Fatalf("own result: %d %s", w.Code, w.Body.String())
	}
	// a server-mode watch cannot be fed by the agent
	var sid int64
	e.st.db.QueryRow(`SELECT id FROM watches WHERE url='https://shop.example.com/s'`).Scan(&sid)
	if w := agentPostResult(e, tokA, map[string]any{"watch_id": sid, "status": "ok", "rendered_html": jsonldPage("1.00")}); w.Code != http.StatusConflict {
		t.Fatalf("server-mode watch accepted: %d", w.Code)
	}
}

// ---- same path as the server fetcher ----

func TestAgentResultMatchesServerFetcher(t *testing.T) {
	e := newTenv(t, nil)
	tok, uid := agentTok(t, e, "+447700900123")
	const u = "https://www.jet2holidays.com/offer"
	target := p(10000)
	srvID, err := e.st.AddWatch(Watch{UserID: uid, URL: u, Label: "L", TargetPence: target, IntervalMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	agID := agentWatch(t, e, uid, u, target, "")

	ff := newFakeFetcher()
	sch := NewScheduler(e.st, ff, e.clk, e.cfg.BaseURL)
	sch.jitter = func(time.Duration) time.Duration { return 0 }
	snd := NewSender(e.st, e.msgr, e.clk, 200)
	e.srv.agent.sched.jitter = func(time.Duration) time.Duration { return 0 }

	for i, price := range []string{"120.00", "120.00", "110.00", "99.00", "95.00", "105.00", "99.00"} {
		html := jsonldPage(price)
		ff.set(u, &Page{Status: 200, Header: http.Header{}, Body: []byte(html)})
		if sch.RunDue(context.Background()) != 1 {
			t.Fatalf("step %d: server watch not run", i)
		}
		jobs := getJobs(t, e, tok)
		if len(jobs) != 1 || jobs[0].WatchID != agID {
			t.Fatalf("step %d: jobs %+v", i, jobs)
		}
		w := agentPostResult(e, tok, map[string]any{"watch_id": agID, "version": jobs[0].Version, "status": "ok", "rendered_html": html,
			"final_url": u, "http_status": 200, "timings": map[string]any{"load_ms": 1200, "idle_ms": 300}})
		if w.Code != 200 {
			t.Fatalf("step %d: result %d %s", i, w.Code, w.Body.String())
		}
		snd.RunOnce(context.Background())
		e.clk.Advance(time.Hour + time.Minute)
	}
	sw, _ := e.st.WatchByID(srvID, uid)
	aw, _ := e.st.WatchByID(agID, uid)
	if sw.Status != aw.Status || *sw.LastPricePence != *aw.LastPricePence || *sw.AlertBase != *aw.AlertBase ||
		sw.LastMethod != aw.LastMethod || sw.Basis != aw.Basis || sw.Currency != aw.Currency || sw.Failures != aw.Failures {
		t.Fatalf("watch state differs:\n%+v\n%+v", sw, aw)
	}
	sh, _ := e.st.History(uid, srvID, 100)
	ah, _ := e.st.History(uid, agID, 100)
	if len(sh) != 7 || len(ah) != 7 {
		t.Fatalf("history %d %d", len(sh), len(ah))
	}
	for i := range sh {
		if sh[i].PricePence != ah[i].PricePence || sh[i].Method != ah[i].Method || sh[i].Basis != ah[i].Basis || sh[i].Currency != ah[i].Currency {
			t.Fatalf("history row %d differs: %+v %+v", i, sh[i], ah[i])
		}
	}
	// Same alerts (kind, price, old, target), as outbox rows and as delivered messages.
	rows := func(id int64) string {
		var sb strings.Builder
		rs, err := e.st.db.Query(`SELECT kind, price_pence, COALESCE(old_pence,-1), COALESCE(target_pence,-1), currency, status, text FROM outbox WHERE watch_id=? ORDER BY id`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		for rs.Next() {
			var k, c, st, tx string
			var pr, o, tg int64
			rs.Scan(&k, &pr, &o, &tg, &c, &st, &tx)
			fmt.Fprintf(&sb, "%s %d %d %d %s %s %q\n", k, pr, o, tg, c, st, tx)
		}
		return sb.String()
	}
	if rs, ra := rows(srvID), rows(agID); rs != ra || rs == "" {
		t.Fatalf("outbox differs:\n%s\n---\n%s", rs, ra)
	}
	if n := len(e.msgr.Sent()); n != 10 { // 5 alerts per watch
		t.Fatalf("sent %d", n)
	}
	// the dashboard notification path is shared as well
	e.st.UseDashboardAlerts(true)
}

func TestAgentResultSelectorMatchesServerFetcher(t *testing.T) {
	e := newTenv(t, nil)
	tok, uid := agentTok(t, e, "+447700900123")
	const u = "https://www.tui.co.uk/holiday"
	srvID, _ := e.st.AddWatch(Watch{UserID: uid, URL: u, Selector: ".price-box .amount", IntervalMin: 60})
	agID := agentWatch(t, e, uid, u, nil, ".price-box .amount")
	ff := newFakeFetcher()
	ff.set(u, &Page{Status: 200, Header: http.Header{}, Body: []byte(selectorPage)})
	sch := NewScheduler(e.st, ff, e.clk, e.cfg.BaseURL)
	sch.RunDue(context.Background())
	jobs := getJobs(t, e, tok)
	if len(jobs) != 1 || jobs[0].Selector != ".price-box .amount" {
		t.Fatalf("jobs %+v", jobs)
	}
	if w := agentPostResult(e, tok, map[string]any{"watch_id": agID, "status": "ok", "rendered_html": selectorPage}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	sw, _ := e.st.WatchByID(srvID, uid)
	aw, _ := e.st.WatchByID(agID, uid)
	if aw.LastPricePence == nil || *aw.LastPricePence != 129900 || *aw.LastPricePence != *sw.LastPricePence || aw.LastMethod != "selector" || aw.Basis != BasisPerPerson {
		t.Fatalf("%+v vs %+v", aw, sw)
	}
}

func TestAgentResultFailuresAndBackoff(t *testing.T) {
	e := newTenv(t, nil)
	tok, uid := agentTok(t, e, "+447700900123")
	id := agentWatch(t, e, uid, "https://www.easyjet.com/p", nil, "")
	now := e.clk.Now().Unix()

	// blocked: recorded as blocked, nothing to extract, quiet for at least 6 hours
	if w := agentPostResult(e, tok, map[string]any{"watch_id": id, "status": "blocked", "failure_class": "challenge"}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	wt, _ := e.st.WatchByID(id, uid)
	if wt.Status != "blocked" || wt.NextCheckAt < now+int64(6*time.Hour/time.Second) || wt.Failures != 1 {
		t.Fatalf("%+v", wt)
	}
	if len(getJobs(t, e, tok)) != 0 {
		t.Fatal("blocked watch offered again at once")
	}
	// transient classes map to fixed reasons; free text from the agent is never stored
	for class, want := range map[string]string{"dns": "look up", "connect": "connect", "tls": "secure-connection", "timeout": "too long", "nav_error": "could not open", "bogus https://x.example/?a=1": "had a problem"} {
		e.st.db.Exec(`UPDATE watches SET next_check_at=0 WHERE id=?`, id)
		agentPostResult(e, tok, map[string]any{"watch_id": id, "status": "error", "failure_class": class})
		wt, _ = e.st.WatchByID(id, uid)
		if wt.Status != "error" || !strings.Contains(wt.LastError, want) || strings.Contains(wt.LastError, "x.example") {
			t.Errorf("%s: %q %q", class, wt.Status, wt.LastError)
		}
	}
	// a challenge page returned as "ok" is still caught by the shared checker
	e.st.db.Exec(`UPDATE watches SET next_check_at=0 WHERE id=?`, id)
	agentPostResult(e, tok, map[string]any{"watch_id": id, "status": "ok", "rendered_html": `<html><title>Just a moment...</title><div id="cf-chl-widget"></div></html>`})
	wt, _ = e.st.WatchByID(id, uid)
	if wt.Status != "blocked" {
		t.Fatalf("challenge html: %q", wt.Status)
	}
	// ok with HTTP 403 reported
	agentPostResult(e, tok, map[string]any{"watch_id": id, "status": "ok", "http_status": 403, "rendered_html": jsonldPage("10.00")})
	wt, _ = e.st.WatchByID(id, uid)
	if wt.Status != "blocked" {
		t.Fatalf("403: %q", wt.Status)
	}
}

func TestAgentResultFinalURLHostCheck(t *testing.T) {
	cases := []struct {
		watch, final string
		ok           bool
	}{
		{"https://www.jet2.com/p", "https://www.jet2.com/other?x=1", true},
		{"https://jet2.com/p", "https://www.jet2.com/p", true},
		{"https://www.tui.co.uk/p", "https://book.tui.co.uk/x", true},
		{"https://www.easyjet.com/p", "https://www.easyjet.com.evil.example/p", false},
		{"https://www.jet2.com/p", "https://example.com/", false},
		{"https://www.jet2.com/p", "https://co.uk/", false},
		{"https://shop.co.uk/p", "https://other.co.uk/p", false},
		{"https://www.jet2.com/p", "javascript:alert(1)", false},
		{"https://www.jet2.com/p", "chrome-error://chromewebdata/", false},
		{"https://www.jet2.com/p", "file:///etc/passwd", false},
		{"https://www.jet2.com/p", "", true},
	}
	for _, c := range cases {
		if got := finalURLOK(c.watch, c.final); got != c.ok {
			t.Errorf("%s -> %s: %v want %v", c.watch, c.final, got, c.ok)
		}
	}
	e := newTenv(t, nil)
	tok, uid := agentTok(t, e, "+447700900123")
	id := agentWatch(t, e, uid, "https://www.jet2.com/p", nil, "")
	agentPostResult(e, tok, map[string]any{"watch_id": id, "status": "ok", "final_url": "https://example.com/", "rendered_html": jsonldPage("10.00")})
	wt, _ := e.st.WatchByID(id, uid)
	if wt.Status != "error" || wt.LastPricePence != nil || !strings.Contains(wt.LastError, "different site") {
		t.Fatalf("%+v", wt)
	}
}

func TestAgentResultStaleVersionDropped(t *testing.T) {
	e := newTenv(t, nil)
	tok, uid := agentTok(t, e, "+447700900123")
	id := agentWatch(t, e, uid, "https://www.jet2.com/p", nil, "")
	jobs := getJobs(t, e, tok)
	// the owner edits the target after the job was issued
	if err := e.st.SetTarget(uid, id, p(5000)); err != nil {
		t.Fatal(err)
	}
	v := jobs[0].Version
	if w := agentPostResult(e, tok, map[string]any{"watch_id": id, "version": v, "status": "ok", "rendered_html": jsonldPage("10.00")}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	h, _ := e.st.History(uid, id, 10)
	if len(h) != 0 {
		t.Fatal("stale result recorded")
	}
	// paused watches record nothing either
	e.st.SetPaused(uid, id, true)
	cur, _ := e.st.WatchByID(id, uid)
	agentPostResult(e, tok, map[string]any{"watch_id": id, "version": cur.Version, "status": "ok", "rendered_html": jsonldPage("10.00")})
	if h, _ := e.st.History(uid, id, 10); len(h) != 0 {
		t.Fatal("paused watch recorded")
	}
}

// ---- scheduler and view ----

type tripFetcher struct{ calls int }

func (f *tripFetcher) Fetch(context.Context, string, *Conditional) (*Page, error) {
	f.calls++
	return nil, errors.New("must not be called")
}

func TestSchedulerNeverFetchesAgentWatches(t *testing.T) {
	e := newTenv(t, nil)
	_, uid := agentTok(t, e, "+447700900123")
	agentWatch(t, e, uid, "https://www.jet2.com/p", nil, "")
	f := &tripFetcher{}
	sch := NewScheduler(e.st, f, e.clk, e.cfg.BaseURL)
	e.clk.Advance(48 * time.Hour)
	if n := sch.RunDue(context.Background()); n != 0 || f.calls != 0 {
		t.Fatalf("ran %d, fetched %d", n, f.calls)
	}
	// switching back to the server makes it fetchable again
	ws, _ := e.st.ListWatches(uid)
	e.st.SetFetchMode(uid, ws[0].ID, FetchServer)
	if n := sch.RunDue(context.Background()); n != 1 || f.calls != 1 {
		t.Fatalf("after switch: ran %d, fetched %d", n, f.calls)
	}
}

func TestAgentOfflineShownOnDashboard(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700 900123")
	uid, _ := e.st.UserID("+447700900123")
	b.do("POST", "/watches", url.Values{"csrf": {csrf}, "url": {"https://www.jet2.com/holiday"}, "interval": {"60"}, "fetch": {"agent"}})
	ws, _ := e.st.ListWatches(uid)
	if len(ws) != 1 {
		t.Fatal("watch not added")
	}
	if m, _ := e.st.FetchModeOf(ws[0].ID); m != FetchAgent {
		t.Fatalf("mode %q", m)
	}
	d := b.do("GET", "/", nil).Body.String()
	if strings.Contains(d, "Agent offline") || !strings.Contains(d, "home agent") {
		t.Fatal("offline too early")
	}
	e.clk.Advance(2*time.Hour + 59*time.Minute)
	if strings.Contains(b.do("GET", "/", nil).Body.String(), "Agent offline") {
		t.Fatal("offline before 3 intervals")
	}
	e.clk.Advance(2 * time.Minute)
	if !strings.Contains(b.do("GET", "/", nil).Body.String(), "Agent offline") {
		t.Fatal("not offline after 3 intervals")
	}
	if !strings.Contains(b.do("GET", fmt.Sprintf("/watches/%d", ws[0].ID), nil).Body.String(), "Agent offline") {
		t.Fatal("watch page does not show it")
	}
	// a deliberate back-off after a challenge is not "offline"
	e.st.RecordFailure(ws[0].ID, "blocked", "x", 1, e.clk.Now().Add(6*time.Hour).Unix(), nil, nil)
	e.clk.Advance(5 * time.Hour)
	if strings.Contains(b.do("GET", "/", nil).Body.String(), "Agent offline") {
		t.Fatal("back-off shown as offline")
	}
}

func TestFetchSelectOnFormsAndExistingWatchesUnchanged(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700 900123")
	uid, _ := e.st.UserID("+447700900123")
	b.do("POST", "/watches", url.Values{"csrf": {csrf}, "url": {"https://shop.example.com/a"}, "interval": {"60"}}) // no fetch field
	ws, _ := e.st.ListWatches(uid)
	if m, _ := e.st.FetchModeOf(ws[0].ID); m != FetchServer {
		t.Fatalf("default mode %q", m)
	}
	d := b.do("GET", "/", nil).Body.String()
	for _, want := range []string{`name="fetch"`, "This server", "My home agent", "jet2.com", "easyjet.com"} {
		if !strings.Contains(d, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
	// edit to agent and back
	path := fmt.Sprintf("/watches/%d/edit", ws[0].ID)
	b.do("POST", path, url.Values{"csrf": {csrf}, "interval": {"60"}, "fetch": {"agent"}})
	if m, _ := e.st.FetchModeOf(ws[0].ID); m != FetchAgent {
		t.Fatal("edit to agent failed")
	}
	// an unknown value leaves it alone
	b.do("POST", path, url.Values{"csrf": {csrf}, "interval": {"60"}, "fetch": {"weird"}})
	if m, _ := e.st.FetchModeOf(ws[0].ID); m != FetchAgent {
		t.Fatal("unknown value changed the mode")
	}
	b.do("POST", path, url.Values{"csrf": {csrf}, "interval": {"60"}, "fetch": {"server"}})
	if m, _ := e.st.FetchModeOf(ws[0].ID); m != FetchServer {
		t.Fatal("edit to server failed")
	}
	// another user cannot change it
	b2 := e.browser()
	c2 := b2.login("07700 900999")
	if w := b2.do("POST", path, url.Values{"csrf": {c2}, "fetch": {"agent"}}); w.Code != 404 {
		t.Fatalf("foreign edit: %d", w.Code)
	}
	for host, want := range map[string]bool{"www.jet2.com": true, "jet2holidays.com": true, "www.tui.co.uk": true, "easyjet.com": true, "nottui.co.uk": false, "example.com": false} {
		if AgentSuggested(host) != want {
			t.Errorf("AgentSuggested(%s) != %v", host, want)
		}
	}
}

// ---- CLI ----

func TestAgentTokenCLI(t *testing.T) {
	env, dir := adduserEnv(t)
	var out bytes.Buffer
	if err := AgentToken(envMap(env), "07700 900123", AgentTokenOpts{}, &out); err != nil {
		t.Fatal(err)
	}
	tok := regexpAgentTok.FindString(out.String())
	if tok == "" || !strings.Contains(out.String(), "treat it like a password") {
		t.Fatal(out.String())
	}
	db, _ := OpenDB(dir)
	defer db.Close()
	st := NewStore(db, SystemClock())
	uid, _ := st.UserID("+447700900123")
	if _, got, err := st.AgentByHash(hashToken(tok)); err != nil || got != uid {
		t.Fatal(got, err)
	}
	var st2 bytes.Buffer
	if err := AgentToken(envMap(env), "+447700900123", AgentTokenOpts{Status: true}, &st2); err != nil || strings.Contains(st2.String(), tok) || !strings.Contains(st2.String(), "active") {
		t.Fatalf("%v %q", err, st2.String())
	}
	if err := AgentToken(envMap(env), "+447700900123", AgentTokenOpts{Revoke: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AgentByHash(hashToken(tok)); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("revoked CLI token valid")
	}
	if err := AgentToken(envMap(env), "+447700900123", AgentTokenOpts{Revoke: true, Status: true}, io.Discard); err == nil {
		t.Fatal("both flags accepted")
	}
}

func TestDeleteUserRemovesAgentTokens(t *testing.T) {
	e := newTenv(t, nil)
	_, uid := agentTok(t, e, "+447700900123")
	if err := e.st.DeleteUser(uid); err != nil {
		t.Fatal(err)
	}
	var n int
	e.st.db.QueryRow(`SELECT COUNT(*) FROM agent_tokens`).Scan(&n)
	if n != 0 {
		t.Fatal("tokens survived account deletion")
	}
}

var regexpAgentTok = regexp.MustCompile(`wwa_[A-Za-z0-9_-]{43}`)
