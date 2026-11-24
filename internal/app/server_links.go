package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func stamp(t time.Time) string { return t.In(london).Format("2 Jan 2006, 15:04") }

func stampPtr(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return stamp(*t)
}

// plView is what the dashboard shows about a permanent link: never the token.
type plView struct {
	Exists  bool
	Created string
	Used    string
}

func (s *Server) personalLinkView(uid int64) plView {
	info, err := s.store.PersonalLinkStatus(uid)
	if err != nil || !info.Exists {
		return plView{}
	}
	return plView{Exists: true, Created: stamp(info.CreatedAt), Used: stampPtr(info.LastUsedAt)}
}

// handlePersonalLink logs in with a permanent personal link. It consumes
// nothing, so link-preview bots cannot burn it, and needs no button press.
// Failures are rate limited per IP and look exactly like an unknown path.
func (s *Server) handlePersonalLink(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet { // HEAD must not start a session
		http.NotFound(w, r)
		return
	}
	ip, now := clientIP(r), s.clock.Now()
	if s.plLimit.Blocked(ip, now) {
		h.Set("Retry-After", "3600")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	tok := r.PathValue("token")
	uid, err := int64(0), ErrInvalidToken
	if len(tok) > 0 && len(tok) <= 128 {
		uid, err = s.store.UsePersonalLink(hashToken(tok))
	}
	if errors.Is(err, ErrInvalidToken) {
		s.plLimit.Allow(ip, now)
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("personal link", "err", RedactPII(err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sess, _, err := s.auth.StartSession(uid)
	if err != nil {
		slog.Error("personal link session", "err", RedactPII(err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setCookie(w, s.sessionCookieName(), sess, int(s.cfg.SessionLifetime().Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleRevokeOwnLink(w http.ResponseWriter, r *http.Request, sess *Session) {
	if _, err := s.store.RevokePersonalLink(sess.UserID); err != nil {
		s.message(w, r, 500, "Error", "Could not revoke the link.")
		return
	}
	http.Redirect(w, r, "/?m=link_revoked", http.StatusSeeOther)
}

// ---- admin ----

// withAdmin serves admins only. Everyone else, logged in or not, gets the
// generic 404. POSTs need the CSRF token. Role is read from the database on
// every request.
func (s *Server) withAdmin(h sessHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.session(r)
		if sess == nil || !sess.IsAdmin {
			http.NotFound(w, r)
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

type adminRow struct {
	ID                                 int64
	Phone, Name, Created, Login, Link  string
	Watches                            int
	Alerts                             string
	Disabled, IsAdmin, HasLink, IsSelf bool
}

type adminEventView struct{ When, Actor, Action, Target string }

var adminFlashes = map[string]string{
	"added": "Account added.", "revoked": "Permanent link revoked.", "disabled": "Account disabled and its sessions ended.",
	"enabled": "Account enabled.", "deleted": "Account deleted.",
}

func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, sess *Session, status int, errMsg, flash string) {
	users, err := s.store.AdminUsers()
	if err != nil {
		s.message(w, r, 500, "Error", "Something went wrong.")
		return
	}
	d := pageData{Title: "Accounts", UserID: sess.UserID, Phone: sess.Phone, CSRF: sess.CSRF, LoggedIn: true, Error: errMsg, Flash: flash, MaxUsers: s.cfg.UserCap(), Count: len(users)}
	for _, u := range users {
		row := adminRow{ID: u.ID, Phone: MaskPhone(u.Phone), Name: u.Name, Created: stamp(u.Created), Login: stamp(u.LastLogin),
			Watches: u.Watches, Disabled: u.Disabled, IsAdmin: u.IsAdmin, HasLink: u.LinkExists, IsSelf: u.ID == sess.UserID, Alerts: "off"}
		if u.AlertsOn {
			row.Alerts = "on"
		}
		if u.LinkExists {
			row.Link = "last used " + stampPtr(u.LinkUsed)
		}
		d.AdminRows = append(d.AdminRows, row)
	}
	evs, _ := s.store.AdminEvents(50)
	for _, e := range evs {
		d.AdminEvents = append(d.AdminEvents, adminEventView{stamp(e.At), e.Actor, e.Action, e.Target})
	}
	s.render(w, status, "admin_users", d)
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request, sess *Session) {
	s.renderAdmin(w, r, sess, http.StatusOK, "", adminFlashes[r.URL.Query().Get("m")])
}

func (s *Server) handleAdminAdd(w http.ResponseWriter, r *http.Request, sess *Session) {
	p, err := NormalisePhone(r.PostFormValue("phone"))
	if err != nil {
		s.renderAdmin(w, r, sess, http.StatusBadRequest, "That is not a valid phone number. Use international format, for example +447700900123.", "")
		return
	}
	uid, err := s.store.CreateUserCapped(p, r.PostFormValue("name"), s.dashboardOnly(), s.cfg.UserCap())
	switch {
	case errors.Is(err, ErrUserExists):
		s.renderAdmin(w, r, sess, http.StatusConflict, "An account with that number already exists.", "")
		return
	case errors.Is(err, ErrUserCap):
		s.renderAdmin(w, r, sess, http.StatusConflict, fmt.Sprintf("The limit of %d accounts has been reached (WW_MAX_USERS).", s.cfg.UserCap()), "")
		return
	case err != nil:
		slog.Error("admin add account", "err", RedactPII(err.Error()))
		s.message(w, r, 500, "Error", "Could not add the account.")
		return
	}
	s.audit(sess, ActAddAccount, uid, p)
	http.Redirect(w, r, "/admin/users?m=added", http.StatusSeeOther)
}

func (s *Server) audit(sess *Session, action string, target int64, phone string) {
	if err := s.store.LogAdminEvent(sess.UserID, action, target, MaskPhone(phone)); err != nil {
		slog.Error("admin audit", "err", RedactPII(err.Error()))
	}
}

// target loads the account named in the path; it writes the response and returns false if absent.
func (s *Server) adminTarget(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		var phone string
		if phone, err = s.store.UserPhone(id); err == nil {
			return id, phone, true
		}
	}
	http.NotFound(w, r)
	return 0, "", false
}

func (s *Server) handleAdminLink(w http.ResponseWriter, r *http.Request, sess *Session) {
	id, phone, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	if id == sess.UserID {
		s.renderAdmin(w, r, sess, http.StatusForbidden, "You cannot create or rotate your own permanent link here. Use the command line: webwatcher permalink.", "")
		return
	}
	raw, hash := newToken()
	if err := s.store.SetPersonalLink(id, hash); err != nil {
		slog.Error("admin link", "err", RedactPII(err.Error()))
		s.message(w, r, 500, "Error", "Could not create the link.")
		return
	}
	s.audit(sess, ActLinkCreate, id, phone)
	// Shown once, on this response only.
	d := pageData{Title: "Permanent link", UserID: sess.UserID, Phone: sess.Phone, CSRF: sess.CSRF, LoggedIn: true,
		Link: strings.TrimRight(s.cfg.BaseURL, "/") + "/p/" + raw, Heading: MaskPhone(phone)}
	s.render(w, http.StatusOK, "admin_link", d)
}

func (s *Server) handleAdminRevoke(w http.ResponseWriter, r *http.Request, sess *Session) {
	id, phone, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	if _, err := s.store.RevokePersonalLink(id); err != nil {
		s.message(w, r, 500, "Error", "Could not revoke the link.")
		return
	}
	s.audit(sess, ActLinkRevoke, id, phone)
	http.Redirect(w, r, "/admin/users?m=revoked", http.StatusSeeOther)
}

func (s *Server) handleAdminDisable(disable bool) sessHandler {
	return func(w http.ResponseWriter, r *http.Request, sess *Session) {
		id, phone, ok := s.adminTarget(w, r)
		if !ok {
			return
		}
		if disable && id == sess.UserID {
			s.renderAdmin(w, r, sess, http.StatusForbidden, "You cannot disable your own account.", "")
			return
		}
		err := s.store.SetDisabled(id, disable)
		if errors.Is(err, ErrLastAdmin) {
			s.renderAdmin(w, r, sess, http.StatusForbidden, "You cannot disable the last enabled admin.", "")
			return
		}
		if err != nil {
			s.message(w, r, 500, "Error", "Could not change the account.")
			return
		}
		act, m := ActEnable, "enabled"
		if disable {
			act, m = ActDisable, "disabled"
		}
		s.audit(sess, act, id, phone)
		http.Redirect(w, r, "/admin/users?m="+m, http.StatusSeeOther)
	}
}

func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request, sess *Session) {
	id, phone, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	if id == sess.UserID {
		s.renderAdmin(w, r, sess, http.StatusForbidden, "You cannot delete your own account here. Use Account, Delete account.", "")
		return
	}
	if r.PostFormValue("confirm") != "1" {
		s.renderAdmin(w, r, sess, http.StatusBadRequest, "Tick the confirmation box to delete an account.", "")
		return
	}
	err := s.store.DeleteUserGuarded(id)
	if errors.Is(err, ErrLastAdmin) {
		s.renderAdmin(w, r, sess, http.StatusForbidden, "You cannot delete the last enabled admin.", "")
		return
	}
	if err != nil {
		s.message(w, r, 500, "Error", "Could not delete the account.")
		return
	}
	s.audit(sess, ActDeleteAccount, id, phone)
	http.Redirect(w, r, "/admin/users?m=deleted", http.StatusSeeOther)
}
