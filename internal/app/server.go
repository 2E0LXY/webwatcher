package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // static binary: do not depend on the host zoneinfo
)

//go:embed web/*.html web/style.css
var webFS embed.FS

const (
	maxFormBytes = 64 << 10
	preCookie    = "ww_pre"
)

var Version = "dev"

var london = func() *time.Location {
	l, err := time.LoadLocation("Europe/London")
	if err != nil {
		return time.UTC
	}
	return l
}()

// Server is the HTTP application.
type Server struct {
	cfg   Config
	store *Store
	auth  *Auth
	msgr  Messenger
	clock Clock
	db    interface{ Ping() error }
	pages map[string]*template.Template
	bg    sync.WaitGroup // background login sends (tests wait on it)
}

func NewServer(cfg Config, store *Store, auth *Auth, msgr Messenger, clock Clock) (*Server, error) {
	s := &Server{cfg: cfg, store: store, auth: auth, msgr: msgr, clock: clock, db: store.db, pages: map[string]*template.Template{}}
	for _, p := range []string{"login", "sent", "confirm", "dashboard", "watch", "account", "dev", "message"} {
		t, err := template.ParseFS(webFS, "web/layout.html", "web/"+p+".html")
		if err != nil {
			return nil, err
		}
		s.pages[p] = t
	}
	return s, nil
}

// Wait blocks until background login sends have finished (used by tests and shutdown).
func (s *Server) Wait() { s.bg.Wait() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginPost)
	mux.HandleFunc("GET /auth/{token}", s.handleAuthConfirm)
	mux.HandleFunc("POST /auth/{token}", s.handleAuthConsume)
	mux.HandleFunc("POST /logout", s.withSession(s.handleLogout))
	mux.HandleFunc("POST /watches", s.withSession(s.handleAddWatch))
	mux.HandleFunc("GET /watches/{id}", s.withSession(s.handleWatch))
	mux.HandleFunc("POST /watches/{id}/edit", s.withSession(s.handleEditWatch))
	mux.HandleFunc("POST /watches/{id}/pause", s.withSession(s.handlePause(true)))
	mux.HandleFunc("POST /watches/{id}/resume", s.withSession(s.handlePause(false)))
	mux.HandleFunc("POST /watches/{id}/delete", s.withSession(s.handleDeleteWatch))
	mux.HandleFunc("POST /watches/{id}/confirm-basis", s.withSession(s.handleConfirmBasis))
	mux.HandleFunc("GET /account", s.withSession(s.handleAccount))
	mux.HandleFunc("POST /account/delete", s.withSession(s.handleDeleteAccount))
	mux.HandleFunc("POST /account/logout-everywhere", s.withSession(s.handleLogoutEverywhere))
	mux.HandleFunc("POST /account/alerts", s.withSession(s.handleToggleAlerts))
	mux.HandleFunc("GET /hook/meta", s.handleMetaVerify)
	mux.HandleFunc("POST /hook/meta", s.handleMetaPost)
	mux.HandleFunc("POST /hook/twilio", s.handleTwilioPost)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /dev", s.handleDev)
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.staticHandler()))
	return s.middleware(mux)
}

func (s *Server) staticHandler() http.Handler {
	fs := http.FileServerFS(webFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "style.css" {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/web/style.css"
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fs.ServeHTTP(w, r2)
	})
}

// ---- middleware ----

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		if s.cfg.SecureCookies() {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		sw := &statusWriter{ResponseWriter: w, status: 200}
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "path", logPath(r), "panic", RedactPII(fmt.Sprint(rec)))
				if sw.status == 200 {
					http.Error(sw, "internal error", http.StatusInternalServerError)
				}
			}
		}()
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		next.ServeHTTP(sw, r.WithContext(ctx))
		slog.Debug("request", "method", r.Method, "path", logPath(r), "status", sw.status)
	})
}

// logPath hides magic-link tokens.
func logPath(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/auth/") {
		return "/auth/[token]"
	}
	return r.URL.Path
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// Only trust X-Forwarded-For from the local reverse proxy (Caddy).
	if ip, err := net.ResolveIPAddr("ip", host); err == nil && ip.IP.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if v := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(v) != nil {
				return v
			}
		}
	}
	return host
}

// ---- cookies and CSRF ----

func (s *Server) sessionCookieName() string {
	if s.cfg.SecureCookies() {
		return "__Host-ww_session"
	}
	return "ww_session"
}

func (s *Server) setCookie(w http.ResponseWriter, name, val string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: val, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

func randomString() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// preCSRF returns the double-submit token for forms shown before login,
// creating its cookie if needed.
func (s *Server) preCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(preCookie); err == nil && len(c.Value) >= 16 && len(c.Value) <= 64 {
		return c.Value
	}
	v := randomString()
	s.setCookie(w, preCookie, v, 3600)
	return v
}

func (s *Server) checkPreCSRF(r *http.Request) bool {
	c, err := r.Cookie(preCookie)
	return err == nil && c.Value != "" && ctEqual(c.Value, r.PostFormValue("csrf"))
}

func (s *Server) parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		s.message(w, r, http.StatusRequestEntityTooLarge, "Bad request", "That form could not be read.")
		return false
	}
	return true
}

type sessHandler func(w http.ResponseWriter, r *http.Request, sess *Session)

func (s *Server) session(r *http.Request) *Session {
	c, err := r.Cookie(s.sessionCookieName())
	if err != nil {
		return nil
	}
	sess, err := s.auth.Session(c.Value)
	if err != nil {
		return nil
	}
	return sess
}

// withSession requires a login and, for POSTs, a valid CSRF token.
func (s *Server) withSession(h sessHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.session(r)
		if sess == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if !s.parseForm(w, r) {
				return
			}
			if !ctEqual(sess.CSRF, r.PostFormValue("csrf")) {
				s.message(w, r, http.StatusForbidden, "Forbidden", "Your form had expired or was invalid. Go back, reload the page and try again.")
				return
			}
		}
		h(w, r, sess)
	}
}

// ---- rendering ----

type intervalOpt struct {
	Minutes  int
	Label    string
	Selected bool
}

type watchView struct {
	ID           int64
	Title        string
	URL          string
	Host         string
	Price        string
	Checked      string
	Status       string // css class: ok, pending, blocked, error, paused, stopped
	StatusTxt    string
	Reason       string
	Target       string
	TargetVal    string
	Method       string
	Interval     string
	Paused       bool
	Stopped      bool
	Selector     string
	Failures     int
	SoldOut      bool
	Kept         bool // a check failed but the last valid price is still shown
	BasisChanged bool
	BasisNote    string
}

type histView struct{ Price, When, Method string }

type formVals struct {
	URL, Label, Selector, Target string
	Interval                     int
	QueryKeys                    string // names of token-like query parameters; needs a confirmation tick
}

type pageData struct {
	Title     string
	Phone     string
	CSRF      string
	Flash     string
	Error     string
	LoggedIn  bool
	Dev       bool
	Version   string
	Form      formVals
	Watches   []watchView
	W         *watchView
	History   []histView
	Intervals []intervalOpt
	Count     int
	Cap       int
	Msgs      []devMsg
	Heading   string
	Body      string
	Token     string
	AlertsOn  bool
	Consented bool
}

type devMsg struct{ Kind, To, Text, Link string }

func (s *Server) render(w http.ResponseWriter, status int, page string, d pageData) {
	d.Version = Version
	d.Dev = s.cfg.Dev
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "layout", d); err != nil {
		slog.Error("render", "page", page, "err", RedactPII(err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) message(w http.ResponseWriter, r *http.Request, status int, heading, body string) {
	d := pageData{Title: heading, Heading: heading, Body: body}
	if sess := s.session(r); sess != nil {
		d.LoggedIn, d.Phone = true, sess.Phone
	}
	s.render(w, status, "message", d)
}

var flashes = map[string]string{
	"added": "Watch added. The first check happens within a minute or so.", "deleted": "Watch deleted.",
	"saved": "Changes saved.", "confirmed": "New price basis confirmed. The baseline has been reset.", "alerts_on": "WhatsApp alerts are on.", "alerts_off": "WhatsApp alerts are off. You will get no more alert messages.", "paused": "Watch paused.", "resumed": "Watch resumed.",
}

func intervalLabel(m int) string {
	switch {
	case m < 60:
		return fmt.Sprintf("%d minutes", m)
	case m == 60:
		return "1 hour"
	case m < 1440:
		return fmt.Sprintf("%d hours", m/60)
	}
	return "24 hours"
}

func intervalOpts(sel int) []intervalOpt {
	var out []intervalOpt
	for _, m := range AllowedIntervals {
		out = append(out, intervalOpt{m, intervalLabel(m), m == sel})
	}
	return out
}

func basisSuffix(b string) string {
	switch b {
	case BasisPerPerson:
		return " per person"
	case BasisTotal:
		return " total"
	}
	return ""
}

func (s *Server) view(w *Watch) watchView {
	v := watchView{ID: w.ID, URL: w.URL, Selector: w.Selector, Method: w.LastMethod, Interval: intervalLabel(w.IntervalMin),
		Paused: w.Paused, Reason: w.LastError, Failures: w.Failures, Price: "–", Checked: "never", Target: "–"}
	if u, err := url.Parse(w.URL); err == nil {
		v.Host = u.Hostname()
	}
	v.Title = w.Label
	if v.Title == "" {
		v.Title = v.Host
	}
	if w.LastPricePence != nil {
		v.Price = FormatPence(*w.LastPricePence, w.Currency)
	}
	if w.TargetPence != nil {
		v.Target = FormatPence(*w.TargetPence, w.Currency)
		v.TargetVal = fmt.Sprintf("%d.%02d", *w.TargetPence/100, *w.TargetPence%100)
	}
	if w.LastCheckedAt != nil {
		v.Checked = time.Unix(*w.LastCheckedAt, 0).In(london).Format("2 Jan 2006, 15:04")
	}
	v.Stopped = w.Failures >= MaxFailures
	v.SoldOut = w.SoldOut
	v.Kept = w.LastPricePence != nil && (w.Status == "error" || w.Status == "noprice")
	if w.LastPricePence != nil {
		v.Price += basisSuffix(w.Basis)
	}
	if w.BasisChanged && w.ObsPence != nil {
		v.BasisChanged = true
		v.BasisNote = fmt.Sprintf("Basis changed: the page now shows %s%s, but the last price you were tracking was %s%s. These cannot be compared, so no alert was sent. If the new figure is the one you want, confirm it to reset the baseline.",
			FormatPence(*w.ObsPence, w.ObsCurrency), basisSuffix(w.ObsBasis), FormatPence(*w.LastPricePence, w.Currency), basisSuffix(w.Basis))
	}
	switch {
	case w.Paused:
		v.Status, v.StatusTxt = "paused", "Paused"
	case v.Stopped:
		v.Status, v.StatusTxt = "stopped", "Stopped after 10 failed checks"
	case w.BasisChanged:
		v.Status, v.StatusTxt = "error", "Basis changed"
	case w.SoldOut:
		v.Status, v.StatusTxt = "paused", "Sold out"
	case w.Status == "blocked":
		v.Status, v.StatusTxt = "blocked", "Blocked by site"
	case w.Status == "noprice":
		v.Status, v.StatusTxt = "error", "No price found"
	case w.Status == "disallowed":
		v.Status, v.StatusTxt = "blocked", "Disallowed by robots.txt"
	case w.Status == "error":
		v.Status, v.StatusTxt = "error", "Temporary error"
	case w.Status == "ok":
		v.Status, v.StatusTxt = "ok", "OK"
	default:
		v.Status, v.StatusTxt = "pending", "Waiting for first check"
	}
	return v
}

// ---- handlers: auth ----

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.renderDashboard(w, r, sess, http.StatusOK, formVals{Interval: DefaultIntervalMin}, "")
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if s.session(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", pageData{Title: "Log in", CSRF: s.preCSRF(w, r)})
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	if !s.checkPreCSRF(r) {
		s.message(w, r, http.StatusForbidden, "Forbidden", "Your form had expired. Go back, reload the page and try again.")
		return
	}
	if r.PostFormValue("consent") != "1" {
		s.render(w, http.StatusBadRequest, "login", pageData{Title: "Log in", CSRF: s.preCSRF(w, r), Error: "Please tick the box to agree to receive WhatsApp messages from Webwatcher. We cannot send you a login link without it."})
		return
	}
	phone, err := NormalisePhone(r.PostFormValue("phone"))
	if err != nil {
		s.render(w, http.StatusBadRequest, "login", pageData{Title: "Log in", CSRF: s.preCSRF(w, r), Error: "That does not look like a valid WhatsApp number. UK numbers can start with 07 or +44."})
		return
	}
	ip := clientIP(r)
	// The response is identical whether or not the number is known or the
	// request was rate limited, and the send happens off-thread so timing
	// does not differ either.
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.auth.RequestLogin(ctx, phone, ip, true)
	}()
	s.render(w, http.StatusOK, "sent", pageData{Title: "Check WhatsApp"})
}

func (s *Server) handleAuthConfirm(w http.ResponseWriter, r *http.Request) {
	// GET only shows a button: link-preview bots that fetch URLs cannot burn the token.
	s.render(w, http.StatusOK, "confirm", pageData{Title: "Log in", CSRF: s.preCSRF(w, r), Token: r.PathValue("token")})
}

func (s *Server) handleAuthConsume(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	if !s.checkPreCSRF(r) {
		s.message(w, r, http.StatusForbidden, "Forbidden", "Your form had expired. Go back, reload the link and try again.")
		return
	}
	sess, _, err := s.auth.Consume(r.PathValue("token"))
	if err != nil {
		s.message(w, r, http.StatusUnauthorized, "Link not valid", "This login link has expired or has already been used. Request a new one from the login page.")
		return
	}
	s.setCookie(w, s.sessionCookieName(), sess, int(SessionTTL.Seconds()))
	s.setCookie(w, preCookie, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, sess *Session) {
	if c, err := r.Cookie(s.sessionCookieName()); err == nil {
		s.store.DeleteSession(hashToken(c.Value))
	}
	s.setCookie(w, s.sessionCookieName(), "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- handlers: watches ----

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, sess *Session, status int, f formVals, errMsg string) {
	ws, err := s.store.ListWatches(sess.UserID)
	if err != nil {
		slog.Error("list watches", "err", RedactPII(err.Error()))
		s.message(w, r, 500, "Error", "Something went wrong.")
		return
	}
	d := pageData{Title: "Dashboard", Phone: sess.Phone, CSRF: sess.CSRF, LoggedIn: true, Form: f, Error: errMsg,
		Flash: flashes[r.URL.Query().Get("m")], Intervals: intervalOpts(f.Interval), Count: len(ws), Cap: MaxWatchesPerUser,
		AlertsOn: sess.AlertsEnabled, Consented: sess.ConsentAt != nil}
	for _, x := range ws {
		d.Watches = append(d.Watches, s.view(x))
	}
	s.render(w, status, "dashboard", d)
}

func parseTarget(s string) (*int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if len(s) > 15 {
		return nil, fmt.Errorf("target price is not a valid amount")
	}
	p, ok := parseAmount(s)
	if !ok || p > 100_000_000 {
		return nil, fmt.Errorf("target price must be an amount in pounds, such as 499.99")
	}
	return &p, nil
}

func parseInterval(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	for _, a := range AllowedIntervals {
		if a == n {
			return n, true
		}
	}
	return 0, false
}

func (s *Server) handleAddWatch(w http.ResponseWriter, r *http.Request, sess *Session) {
	f := formVals{
		URL: strings.TrimSpace(r.PostFormValue("url")), Label: strings.TrimSpace(r.PostFormValue("label")),
		Selector: strings.TrimSpace(r.PostFormValue("selector")), Target: strings.TrimSpace(r.PostFormValue("target")),
	}
	iv, ok := parseInterval(r.PostFormValue("interval"))
	if !ok {
		iv = DefaultIntervalMin
	}
	f.Interval = iv
	fail := func(msg string) { s.renderDashboard(w, r, sess, http.StatusBadRequest, f, msg) }
	clean, err := ValidateWatchURL(f.URL)
	if err != nil {
		fail(err.Error())
		return
	}
	if keys := SensitiveQueryKeys(clean); len(keys) > 0 && r.PostFormValue("confirm_query") != "1" {
		f.QueryKeys = strings.Join(keys, ", ")
		fail("This URL has query parameters that look like a token, session or booking reference (" + f.QueryKeys +
			"). Webwatcher stores the full URL and sends it to the site on every check, and anyone with access to your account or its backups could see it. Tick the box below to confirm, or remove the parameters.")
		return
	}
	if len([]rune(f.Label)) > 80 {
		fail("The label can be at most 80 characters.")
		return
	}
	if f.Selector != "" {
		if _, err := ParseSelector(f.Selector); err != nil {
			fail("Selector problem: " + err.Error())
			return
		}
	}
	target, err := parseTarget(f.Target)
	if err != nil {
		fail(capitalise(err.Error()) + ".")
		return
	}
	_, err = s.store.AddWatch(Watch{UserID: sess.UserID, URL: clean, Label: f.Label, Selector: f.Selector, TargetPence: target, IntervalMin: iv})
	if err == ErrWatchCap {
		fail(fmt.Sprintf("You can watch at most %d pages. Delete one first.", MaxWatchesPerUser))
		return
	}
	if err != nil {
		slog.Error("add watch", "err", RedactPII(err.Error()))
		fail("Could not save the watch.")
		return
	}
	http.Redirect(w, r, "/?m=added", http.StatusSeeOther)
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *Server) watchFromPath(w http.ResponseWriter, r *http.Request, sess *Session) (*Watch, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		var wt *Watch
		if wt, err = s.store.WatchByID(id, sess.UserID); err == nil {
			return wt, true
		}
	}
	s.message(w, r, http.StatusNotFound, "Not found", "That watch does not exist.")
	return nil, false
}

func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request, sess *Session) {
	wt, ok := s.watchFromPath(w, r, sess)
	if !ok {
		return
	}
	s.renderWatch(w, http.StatusOK, sess, wt, "", flashes[r.URL.Query().Get("m")])
}

func (s *Server) renderWatch(w http.ResponseWriter, status int, sess *Session, wt *Watch, errMsg, flash string) {
	v := s.view(wt)
	hist, _ := s.store.History(wt.UserID, wt.ID, 100)
	d := pageData{Title: v.Title, Phone: sess.Phone, CSRF: sess.CSRF, LoggedIn: true, W: &v, Error: errMsg, Flash: flash,
		Intervals: intervalOpts(wt.IntervalMin)}
	for _, h := range hist {
		d.History = append(d.History, histView{FormatPence(h.PricePence, h.Currency) + basisSuffix(h.Basis), time.Unix(h.CheckedAt, 0).In(london).Format("2 Jan 2006, 15:04"), h.Method})
	}
	s.render(w, status, "watch", d)
}

func (s *Server) handleEditWatch(w http.ResponseWriter, r *http.Request, sess *Session) {
	wt, ok := s.watchFromPath(w, r, sess)
	if !ok {
		return
	}
	target, err := parseTarget(r.PostFormValue("target"))
	if err != nil {
		s.renderWatch(w, http.StatusBadRequest, sess, wt, capitalise(err.Error())+".", "")
		return
	}
	iv, ok := parseInterval(r.PostFormValue("interval"))
	if !ok {
		iv = wt.IntervalMin
	}
	same := (target == nil && wt.TargetPence == nil) || (target != nil && wt.TargetPence != nil && *target == *wt.TargetPence)
	if !same {
		if err := s.store.SetTarget(sess.UserID, wt.ID, target); err != nil {
			s.message(w, r, 500, "Error", "Could not save.")
			return
		}
	}
	if err := s.store.SetInterval(sess.UserID, wt.ID, iv); err != nil {
		s.message(w, r, 500, "Error", "Could not save.")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/watches/%d?m=saved", wt.ID), http.StatusSeeOther)
}

func (s *Server) handlePause(pause bool) sessHandler {
	return func(w http.ResponseWriter, r *http.Request, sess *Session) {
		wt, ok := s.watchFromPath(w, r, sess)
		if !ok {
			return
		}
		if err := s.store.SetPaused(sess.UserID, wt.ID, pause); err != nil {
			s.message(w, r, 500, "Error", "Could not save.")
			return
		}
		m := "resumed"
		if pause {
			m = "paused"
		}
		dest := "/?m=" + m
		if r.PostFormValue("back") == "watch" {
			dest = fmt.Sprintf("/watches/%d?m=%s", wt.ID, m)
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
	}
}

func (s *Server) handleConfirmBasis(w http.ResponseWriter, r *http.Request, sess *Session) {
	wt, ok := s.watchFromPath(w, r, sess)
	if !ok {
		return
	}
	if err := s.store.ConfirmBasis(sess.UserID, wt.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Redirect(w, r, fmt.Sprintf("/watches/%d", wt.ID), http.StatusSeeOther)
			return
		}
		s.message(w, r, 500, "Error", "Could not save.")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/watches/%d?m=confirmed", wt.ID), http.StatusSeeOther)
}

func (s *Server) handleDeleteWatch(w http.ResponseWriter, r *http.Request, sess *Session) {
	wt, ok := s.watchFromPath(w, r, sess)
	if !ok {
		return
	}
	if err := s.store.DeleteWatch(sess.UserID, wt.ID); err != nil {
		s.message(w, r, 500, "Error", "Could not delete.")
		return
	}
	http.Redirect(w, r, "/?m=deleted", http.StatusSeeOther)
}

// ---- handlers: account, health, dev ----

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request, sess *Session) {
	n, _ := s.store.CountWatches(sess.UserID)
	s.render(w, http.StatusOK, "account", pageData{Title: "Account", Phone: sess.Phone, CSRF: sess.CSRF, LoggedIn: true, Count: n})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request, sess *Session) {
	if strings.TrimSpace(r.PostFormValue("confirm")) != "DELETE" {
		n, _ := s.store.CountWatches(sess.UserID)
		s.render(w, http.StatusBadRequest, "account", pageData{Title: "Account", Phone: sess.Phone, CSRF: sess.CSRF, LoggedIn: true, Count: n,
			Error: "Type DELETE in capitals to confirm."})
		return
	}
	if err := s.store.DeleteUser(sess.UserID); err != nil {
		slog.Error("delete account", "err", RedactPII(err.Error()))
		s.message(w, r, 500, "Error", "Could not delete the account.")
		return
	}
	s.setCookie(w, s.sessionCookieName(), "", -1)
	s.message(w, r, http.StatusOK, "Account deleted", "Your number, watches, price history and sessions have been erased.")
}

// handleLogoutEverywhere revokes every session of the user, this one included.
func (s *Server) handleLogoutEverywhere(w http.ResponseWriter, r *http.Request, sess *Session) {
	if err := s.store.DeleteUserSessions(sess.UserID); err != nil {
		s.message(w, r, 500, "Error", "Could not log you out everywhere.")
		return
	}
	s.setCookie(w, s.sessionCookieName(), "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleToggleAlerts(w http.ResponseWriter, r *http.Request, sess *Session) {
	on := r.PostFormValue("enabled") == "1"
	if err := s.store.SetAlertsEnabled(sess.UserID, on); err != nil {
		if errors.Is(err, ErrNoConsent) {
			s.message(w, r, http.StatusBadRequest, "Consent needed", "We have no record that you agreed to receive WhatsApp messages. Log out, then log in again and tick the agreement box.")
			return
		}
		s.message(w, r, 500, "Error", "Could not save.")
		return
	}
	m := "alerts_off"
	if on {
		m = "alerts_on"
	}
	http.Redirect(w, r, "/?m="+m, http.StatusSeeOther)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AdminToken == "" {
		http.NotFound(w, r)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.Header.Get("X-Admin-Token")
	}
	if !ctEqual(tok, s.cfg.AdminToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="webwatcher"`)
		http.Error(w, "unauthorised", http.StatusUnauthorized)
		return
	}
	status, code := "ok", 200
	if err := s.db.Ping(); err != nil {
		status, code = "database error", 503
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"status": status, "version": Version, "messenger": s.msgr.Name()})
}

func (s *Server) handleDev(w http.ResponseWriter, r *http.Request) {
	lm, ok := s.msgr.(*LogMessenger)
	if !s.cfg.Dev || !ok {
		http.NotFound(w, r)
		return
	}
	var msgs []devMsg
	for _, m := range lm.Recent() {
		d := devMsg{Kind: string(m.Kind), To: m.To, Text: m.Text}
		if m.Kind == KindLogin && len(m.Params) > 0 {
			d.Link = m.Params[0]
		}
		msgs = append(msgs, d)
	}
	s.render(w, http.StatusOK, "dev", pageData{Title: "Dev messages", Msgs: msgs})
}
