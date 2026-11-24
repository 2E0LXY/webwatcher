package app

import (
	"net/url"
	"strings"
	"testing"
)

func TestLoginRequiresConsentCheckbox(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	c := b.csrf("/login")
	if !strings.Contains(b.do("GET", "/login", nil).Body.String(), "I agree to receive WhatsApp messages from Webwatcher about my watches") {
		t.Fatal("consent label missing")
	}
	w := b.do("POST", "/login", url.Values{"csrf": {c}, "phone": {"07700900123"}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "tick the box") {
		t.Fatalf("%d", w.Code)
	}
	e.srv.Wait()
	if len(e.msgr.Sent()) != 0 {
		t.Fatal("login link sent without consent")
	}
}

func TestConsentRecordedWithTimestamp(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	b.login("07700900123")
	var at *int64
	var on int
	e.st.db.QueryRow(`SELECT consent_at, alerts_enabled FROM users`).Scan(&at, &on)
	if at == nil || *at != e.clk.Now().Unix() || on != 1 {
		t.Fatalf("%v %d", at, on)
	}
}

func TestAlertsToggleOnDashboard(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700900123")
	if !strings.Contains(b.do("GET", "/", nil).Body.String(), "Turn alerts off") {
		t.Fatal("toggle missing")
	}
	b.do("POST", "/account/alerts", url.Values{"csrf": {csrf}, "enabled": {"0"}})
	if !strings.Contains(b.do("GET", "/", nil).Body.String(), "Turn alerts on") {
		t.Fatal("not off")
	}
	b.do("POST", "/account/alerts", url.Values{"csrf": {csrf}, "enabled": {"1"}})
	if !strings.Contains(b.do("GET", "/", nil).Body.String(), "Turn alerts off") {
		t.Fatal("not on")
	}
	if w := b.do("POST", "/account/alerts", url.Values{"enabled": {"0"}}); w.Code != 403 {
		t.Fatalf("csrf not required: %d", w.Code)
	}
}

func TestNoConsentCannotEnable(t *testing.T) {
	st, _ := testStore(t)
	if err := st.CreateLoginToken("+447700900123", "h", 60e9, false); err != nil {
		t.Fatal(err)
	}
	uid, _, err := st.ConsumeLoginToken("h")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAlertsEnabled(uid, true); err != ErrNoConsent {
		t.Fatalf("%v", err)
	}
	w, _ := st.AddWatch(Watch{UserID: uid, URL: "https://a.example.com/", IntervalMin: 60})
	got, _ := st.WatchByID(w, uid)
	if got.AlertsOn {
		t.Fatal("alerts on without consent")
	}
}

func TestMaskPhone(t *testing.T) {
	if MaskPhone("+447700900123") != "***123" || MaskPhone("12") != "***" {
		t.Fatal(MaskPhone("+447700900123"))
	}
}

// ---- webhooks ----

func metaBody(id, from, text string) string {
	return `{"entry":[{"changes":[{"value":{"messages":[{"id":"` + id + `","from":"` + from + `","type":"text","text":{"body":"` + text + `"}}]}}]}]}`
}

func hookEnv(t *testing.T) *tenv {
	return newTenv(t, func(c *Config) {
		c.WAAppSecret, c.WAVerifyToken, c.TwToken = "appsecret", "verifytok", "twtoken"
	})
}

func alertsOn(e *tenv, uid int64) bool {
	var on int
	e.st.db.QueryRow(`SELECT alerts_enabled FROM users WHERE id=?`, uid).Scan(&on)
	return on == 1
}

func postMeta(e *tenv, body, sig string) int {
	b := e.browser()
	r := reqWithHeader(e, "POST", "/hook/meta", body, map[string]string{"X-Hub-Signature-256": sig, "Content-Type": "application/json"})
	_ = b
	return r
}

func TestHooks404WhenUnconfigured(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	for _, c := range [][2]string{{"GET", "/hook/meta"}, {"POST", "/hook/meta"}, {"POST", "/hook/twilio"}} {
		if w := b.do(c[0], c[1], nil); w.Code != 404 {
			t.Errorf("%v: %d", c, w.Code)
		}
	}
	// only the Twilio token set: Meta endpoints stay hidden
	e = newTenv(t, func(c *Config) { c.TwToken = "x" })
	if w := e.browser().do("GET", "/hook/meta", nil); w.Code != 404 {
		t.Fatal("meta exposed")
	}
	if w := e.browser().do("POST", "/hook/twilio", nil); w.Code == 404 {
		t.Fatal("twilio hidden although configured")
	}
}

func TestMetaVerifyChallenge(t *testing.T) {
	e := hookEnv(t)
	b := e.browser()
	w := b.do("GET", "/hook/meta?hub.mode=subscribe&hub.verify_token=verifytok&hub.challenge=12345", nil)
	if w.Code != 200 || w.Body.String() != "12345" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	if w := b.do("GET", "/hook/meta?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=1", nil); w.Code != 403 {
		t.Fatalf("%d", w.Code)
	}
	if w := b.do("GET", "/hook/meta?hub.mode=subscribe&hub.challenge=1", nil); w.Code != 403 {
		t.Fatalf("%d", w.Code)
	}
}

func TestMetaStopStartAndReplay(t *testing.T) {
	e := hookEnv(t)
	b := e.browser()
	b.login("07700900123")
	const uid = 1
	body := metaBody("wamid.1", "447700900123", "STOP")
	if code := postMeta(e, body, "sha256=00"); code != 403 {
		t.Fatalf("bad signature accepted: %d", code)
	}
	if code := postMeta(e, body, ""); code != 403 {
		t.Fatalf("missing signature accepted: %d", code)
	}
	if !alertsOn(e, uid) {
		t.Fatal("unsigned request changed state")
	}
	if code := postMeta(e, body, metaSignature("appsecret", []byte(body))); code != 200 {
		t.Fatalf("%d", code)
	}
	if alertsOn(e, uid) {
		t.Fatal("STOP not applied")
	}
	// START with a new id re-enables; replaying the old STOP id must not switch off again
	start := metaBody("wamid.2", "447700900123", " start! ")
	postMeta(e, start, metaSignature("appsecret", []byte(start)))
	if !alertsOn(e, uid) {
		t.Fatal("START not applied")
	}
	postMeta(e, body, metaSignature("appsecret", []byte(body)))
	if !alertsOn(e, uid) {
		t.Fatal("replayed STOP was applied")
	}
	// other words are ignored
	hi := metaBody("wamid.3", "447700900123", "hello")
	postMeta(e, hi, metaSignature("appsecret", []byte(hi)))
	unsub := metaBody("wamid.4", "447700900123", "Unsubscribe")
	postMeta(e, unsub, metaSignature("appsecret", []byte(unsub)))
	if alertsOn(e, uid) {
		t.Fatal("UNSUBSCRIBE not applied")
	}
}

func TestMetaBodyCapAndUnknownNumber(t *testing.T) {
	e := hookEnv(t)
	big := strings.Repeat("a", maxHookBytes+10)
	if code := postMeta(e, big, metaSignature("appsecret", []byte(big))); code != 413 {
		t.Fatalf("%d", code)
	}
	body := metaBody("wamid.9", "447700900999", "STOP")
	if code := postMeta(e, body, metaSignature("appsecret", []byte(body))); code != 200 {
		t.Fatalf("%d", code)
	}
}

func postTwilio(e *tenv, form url.Values, sig string) int {
	return reqWithHeader(e, "POST", "/hook/twilio", form.Encode(), map[string]string{"X-Twilio-Signature": sig, "Content-Type": "application/x-www-form-urlencoded"})
}

func TestTwilioSignatureVector(t *testing.T) {
	// Expected value computed independently (Python hmac) from the documented algorithm.
	form := url.Values{"CallSid": {"CA1234567890ABCDE"}, "Caller": {"+12349013030"}, "Digits": {"1234"}, "From": {"+12349013030"}, "To": {"+18005551212"}}
	got := twilioSignature("12345", "https://mycompany.com/myapp.php?foo=1&bar=2", form)
	if got != "0/KCTR6DLpKmkAf8muzZqo1nDgQ=" {
		t.Fatalf("got %s", got)
	}
}

func TestTwilioStopStart(t *testing.T) {
	e := hookEnv(t)
	b := e.browser()
	b.login("07700900123")
	f := url.Values{"MessageSid": {"SM1"}, "From": {"whatsapp:+447700900123"}, "Body": {"STOP"}}
	if code := postTwilio(e, f, "bogus"); code != 403 {
		t.Fatalf("%d", code)
	}
	sig := twilioSignature("twtoken", "http://ww.test/hook/twilio", f)
	if code := postTwilio(e, f, sig); code != 200 || alertsOn(e, 1) {
		t.Fatalf("stop: %d on=%v", code, alertsOn(e, 1))
	}
	f2 := url.Values{"MessageSid": {"SM2"}, "From": {"whatsapp:+447700900123"}, "Body": {"START"}}
	postTwilio(e, f2, twilioSignature("twtoken", "http://ww.test/hook/twilio", f2))
	if !alertsOn(e, 1) {
		t.Fatal("start not applied")
	}
	// replay of SM1 (validly signed) is ignored
	postTwilio(e, f, sig)
	if !alertsOn(e, 1) {
		t.Fatal("replay applied")
	}
	// tampered parameter breaks the signature
	f3 := url.Values{"MessageSid": {"SM3"}, "From": {"whatsapp:+447700900123"}, "Body": {"STOP"}}
	s3 := twilioSignature("twtoken", "http://ww.test/hook/twilio", f3)
	f3.Set("Body", "STOP ")
	if code := postTwilio(e, f3, s3); code != 403 {
		t.Fatalf("%d", code)
	}
}
