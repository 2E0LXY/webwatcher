package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestMetaMessengerRequestShape(t *testing.T) {
	logs := captureLogs(t)
	const token = "EAAB-super-secret-token"
	var gotPath, gotAuth, gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotCT = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"messages":[{"id":"wamid.x"}]}`))
	}))
	defer srv.Close()
	m := NewMetaMessenger(Config{WAPhoneID: "12345", WAToken: token, WATplLogin: "ww_login", WATplAlert: "ww_alert", WALang: "en_GB"})
	m.baseURL = srv.URL
	err := m.Send(context.Background(), Message{Kind: KindAlert, To: "+447700900123", Text: "x", Params: []string{"Label", "sum", "https://u", "https://d/"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/12345/messages" || gotAuth != "Bearer "+token || gotCT != "application/json" {
		t.Fatalf("%s %s %s", gotPath, gotAuth, gotCT)
	}
	if gotBody["messaging_product"] != "whatsapp" || gotBody["to"] != "447700900123" || gotBody["type"] != "template" {
		t.Fatalf("%v", gotBody)
	}
	tpl := gotBody["template"].(map[string]any)
	if tpl["name"] != "ww_alert" || tpl["language"].(map[string]any)["code"] != "en_GB" {
		t.Fatalf("%v", tpl)
	}
	comp := tpl["components"].([]any)[0].(map[string]any)
	ps := comp["parameters"].([]any)
	if comp["type"] != "body" || len(ps) != 4 || ps[0].(map[string]any)["text"] != "Label" || ps[0].(map[string]any)["type"] != "text" {
		t.Fatalf("%v", comp)
	}
	// login uses the login template
	m.Send(context.Background(), Message{Kind: KindLogin, To: "+447700900123", Params: []string{"https://l"}})
	if gotBody["template"].(map[string]any)["name"] != "ww_login" {
		t.Fatal("login template not used")
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("token in logs")
	}
}

func TestMetaMessengerErrorsNeverLeakToken(t *testing.T) {
	logs := captureLogs(t)
	const token = "EAAB-super-secret-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		// a hostile/echoing server reflecting the bearer token in its message
		io.WriteString(w, `{"error":{"message":"Invalid OAuth access token `+token+`","code":190}}`)
	}))
	m := NewMetaMessenger(Config{WAPhoneID: "1", WAToken: token, WATplLogin: "l", WATplAlert: "a", WALang: "en_GB"})
	m.baseURL = srv.URL
	err := m.Send(context.Background(), Message{Kind: KindAlert, To: "+447700900123", Params: []string{"a"}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "190") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(logs.String(), token) {
		t.Fatal("token leaked")
	}
	srv.Close() // connection error path
	err = m.Send(context.Background(), Message{Kind: KindAlert, To: "+447700900123", Params: []string{"a"}})
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("%v", err)
	}
	// a 500 with a non-JSON body
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500); io.WriteString(w, "oops") }))
	defer srv2.Close()
	m.baseURL = srv2.URL
	if err := m.Send(context.Background(), Message{Kind: KindAlert, To: "+44", Params: nil}); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("%v", err)
	}
}

func TestTwilioMessengerRequestShape(t *testing.T) {
	logs := captureLogs(t)
	const token = "twilio-auth-token-xyz"
	var gotPath, gotUser, gotPass string
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
		r.ParseForm()
		form = r.PostForm
		w.WriteHeader(201)
		w.Write([]byte(`{"sid":"SM1"}`))
	}))
	defer srv.Close()
	m := NewTwilioMessenger(Config{TwSID: "ACabc", TwToken: token, TwFrom: "+14155238886"})
	m.baseURL = srv.URL
	err := m.Send(context.Background(), Message{Kind: KindAlert, To: "+447700900123", Text: "hello there", Params: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/2010-04-01/Accounts/ACabc/Messages.json" || gotUser != "ACabc" || gotPass != token {
		t.Fatalf("%s %s %s", gotPath, gotUser, gotPass)
	}
	if form.Get("From") != "whatsapp:+14155238886" || form.Get("To") != "whatsapp:+447700900123" || form.Get("Body") != "hello there" || form.Get("ContentSid") != "" {
		t.Fatalf("%v", form)
	}
	// content template mode
	m.contentAlert = "HXalert"
	m.Send(context.Background(), Message{Kind: KindAlert, To: "+447700900123", Text: "ignored", Params: []string{"Label", "sum"}})
	if form.Get("ContentSid") != "HXalert" || form.Get("Body") != "" {
		t.Fatalf("%v", form)
	}
	var vars map[string]string
	json.Unmarshal([]byte(form.Get("ContentVariables")), &vars)
	if vars["1"] != "Label" || vars["2"] != "sum" {
		t.Fatalf("%v", vars)
	}
	// login without login content SID falls back to Body
	m.Send(context.Background(), Message{Kind: KindLogin, To: "+447700900123", Text: "login text", Params: []string{"l"}})
	if form.Get("Body") != "login text" {
		t.Fatalf("%v", form)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("token in logs")
	}
}

func TestTwilioErrors(t *testing.T) {
	const token = "tok-secret-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"code":63016,"message":"Outside the window; echoed `+token+`"}`)
	}))
	defer srv.Close()
	m := NewTwilioMessenger(Config{TwSID: "AC1", TwToken: token, TwFrom: "whatsapp:+1415"})
	m.baseURL = srv.URL
	err := m.Send(context.Background(), Message{Kind: KindAlert, To: "+447700900123", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "63016") || strings.Contains(err.Error(), token) {
		t.Fatalf("%v", err)
	}
}

func TestNewMessengerSelection(t *testing.T) {
	if NewMessenger(Config{Messenger: "meta", WAPhoneID: "1", WAToken: "t"}).Name() != "meta" ||
		NewMessenger(Config{Messenger: "twilio", TwSID: "a", TwToken: "b", TwFrom: "c"}).Name() != "twilio" ||
		NewMessenger(Config{Messenger: "log"}).Name() != "log" {
		t.Fatal("selection")
	}
}
