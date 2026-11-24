package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MetaMessenger sends WhatsApp Cloud API template messages.
type MetaMessenger struct {
	phoneID, token string
	tplLogin       string
	tplAlert       string
	lang           string
	baseURL        string // overridable in tests
	client         *http.Client
}

func NewMetaMessenger(c Config) *MetaMessenger {
	return &MetaMessenger{
		phoneID: c.WAPhoneID, token: c.WAToken, tplLogin: c.WATplLogin, tplAlert: c.WATplAlert, lang: c.WALang,
		baseURL: "https://graph.facebook.com/v21.0",
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (m *MetaMessenger) Name() string { return "meta" }

func (m *MetaMessenger) redact(s string) string {
	if m.token != "" {
		s = strings.ReplaceAll(s, m.token, "[redacted]")
	}
	return s
}

func (m *MetaMessenger) Send(ctx context.Context, msg Message) error {
	tpl := m.tplAlert
	if msg.Kind == KindLogin {
		tpl = m.tplLogin
	}
	params := make([]map[string]string, len(msg.Params))
	for i, p := range msg.Params {
		params[i] = map[string]string{"type": "text", "text": p}
	}
	body := map[string]any{
		"messaging_product": "whatsapp",
		"to":                strings.TrimPrefix(msg.To, "+"),
		"type":              "template",
		"template": map[string]any{
			"name":       tpl,
			"language":   map[string]string{"code": m.lang},
			"components": []map[string]any{{"type": "body", "parameters": params}},
		},
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/"+m.phoneID+"/messages", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("meta: %s", m.redact(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("meta: request failed: %s", m.redact(shortErr(err)))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		return fmt.Errorf("meta: HTTP %d: (code %d) %s", resp.StatusCode, e.Error.Code, m.redact(plainParam(e.Error.Message, 200)))
	}
	return fmt.Errorf("meta: HTTP %d", resp.StatusCode)
}
