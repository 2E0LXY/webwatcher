package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TwilioMessenger sends WhatsApp messages via the Twilio Messages API. If a
// content template SID is configured for the message kind it is used (needed
// outside the 24-hour session window); otherwise a free-form Body is sent.
type TwilioMessenger struct {
	sid, token, from           string
	contentLogin, contentAlert string
	baseURL                    string // overridable in tests
	client                     *http.Client
}

func NewTwilioMessenger(c Config) *TwilioMessenger {
	from := c.TwFrom
	if !strings.HasPrefix(from, "whatsapp:") {
		from = "whatsapp:" + from
	}
	return &TwilioMessenger{
		sid: c.TwSID, token: c.TwToken, from: from,
		contentLogin: c.TwContentLogin, contentAlert: c.TwContentAlert,
		baseURL: "https://api.twilio.com",
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (t *TwilioMessenger) Name() string { return "twilio" }

func (t *TwilioMessenger) redact(s string) string {
	for _, secret := range []string{t.token} {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

func (t *TwilioMessenger) Send(ctx context.Context, msg Message) error {
	form := url.Values{}
	form.Set("From", t.from)
	form.Set("To", "whatsapp:"+msg.To)
	content := t.contentAlert
	if msg.Kind == KindLogin {
		content = t.contentLogin
	}
	if content != "" {
		vars := map[string]string{}
		for i, p := range msg.Params {
			vars[strconv.Itoa(i+1)] = p
		}
		vj, _ := json.Marshal(vars)
		form.Set("ContentSid", content)
		form.Set("ContentVariables", string(vj))
	} else {
		form.Set("Body", msg.Text)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.baseURL+"/2010-04-01/Accounts/"+url.PathEscape(t.sid)+"/Messages.json", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("twilio: %s", t.redact(err.Error()))
	}
	req.SetBasicAuth(t.sid, t.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("twilio: request failed: %s", t.redact(shortErr(err)))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &e) == nil && e.Message != "" {
		return fmt.Errorf("twilio: HTTP %d: (code %d) %s", resp.StatusCode, e.Code, t.redact(plainParam(e.Message, 200)))
	}
	return fmt.Errorf("twilio: HTTP %d", resp.StatusCode)
}

// NewMessenger builds the messenger selected by configuration.
func NewMessenger(c Config) Messenger {
	switch c.Messenger {
	case "meta":
		return NewMetaMessenger(c)
	case "twilio":
		return NewTwilioMessenger(c)
	}
	return &LogMessenger{}
}
