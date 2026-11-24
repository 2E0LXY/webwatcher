package app

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const maxHookBytes = 256 << 10

// optCommand classifies an inbound WhatsApp message body. ok is false when it
// is neither an opt-out nor an opt-in keyword.
func optCommand(body string) (on, ok bool) {
	k := strings.ToUpper(strings.TrimFunc(strings.TrimSpace(body), func(r rune) bool {
		return r == '.' || r == '!' || r == ',' || r == ' '
	}))
	switch k {
	case "STOP", "STOPALL", "UNSUBSCRIBE", "CANCEL", "END", "QUIT":
		return false, true
	case "START", "UNSTOP":
		return true, true
	}
	return false, false
}

func (s *Server) applyOptCommand(provider, msgID, from, body string) {
	on, ok := optCommand(body)
	if !ok || msgID == "" {
		return
	}
	phone := "+" + strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(from), "whatsapp:"), "+")
	replay, err := s.store.ApplyInbound(provider, msgID, phone, on)
	if err != nil {
		slog.Error("inbound command", "provider", provider, "err", err)
		return
	}
	if replay {
		slog.Info("inbound replay ignored", "provider", provider)
		return
	}
	slog.Info("inbound opt command applied", "provider", provider, "alerts", on, "number", MaskPhone(phone))
}

// ---- Meta ----

// handleMetaVerify answers Meta's GET subscription challenge.
func (s *Server) handleMetaVerify(w http.ResponseWriter, r *http.Request) {
	if s.cfg.WAVerifyToken == "" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if q.Get("hub.mode") != "subscribe" || !ctEqual(q.Get("hub.verify_token"), s.cfg.WAVerifyToken) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, q.Get("hub.challenge"))
}

func metaSignature(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func (s *Server) handleMetaPost(w http.ResponseWriter, r *http.Request) {
	if s.cfg.WAAppSecret == "" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBytes))
	if err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	got := strings.ToLower(r.Header.Get("X-Hub-Signature-256"))
	if !ctEqual(got, metaSignature(s.cfg.WAAppSecret, body)) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	var p struct {
		Entry []struct {
			Changes []struct {
				Value struct {
					Messages []struct {
						ID   string `json:"id"`
						From string `json:"from"`
						Type string `json:"type"`
						Text struct {
							Body string `json:"body"`
						} `json:"text"`
						Button struct {
							Text string `json:"text"`
						} `json:"button"`
					} `json:"messages"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, e := range p.Entry {
		for _, c := range e.Changes {
			for _, m := range c.Value.Messages {
				text := m.Text.Body
				if m.Type == "button" {
					text = m.Button.Text
				}
				s.applyOptCommand("meta", m.ID, m.From, text)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ---- Twilio ----

// twilioSignature implements Twilio's request validation: HMAC-SHA1 over the
// full URL followed by each POST parameter name and value in sorted order.
func twilioSignature(token, fullURL string, form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(fullURL)
	for _, k := range keys {
		vals := append([]string(nil), form[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			b.WriteString(k)
			b.WriteString(v)
		}
	}
	m := hmac.New(sha1.New, []byte(token))
	m.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) handleTwilioPost(w http.ResponseWriter, r *http.Request) {
	if s.cfg.TwToken == "" {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxHookBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	full := strings.TrimRight(s.cfg.BaseURL, "/") + r.URL.RequestURI()
	if !ctEqual(r.Header.Get("X-Twilio-Signature"), twilioSignature(s.cfg.TwToken, full, r.PostForm)) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	s.applyOptCommand("twilio", r.PostForm.Get("MessageSid"), r.PostForm.Get("From"), r.PostForm.Get("Body"))
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Response></Response>`)
}
