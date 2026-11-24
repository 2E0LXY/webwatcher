// Package app implements webwatcher: a small price watcher with WhatsApp alerts.
package app

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Clock abstracts time so the scheduler and token expiry can be tested.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Config holds all runtime settings. Secrets come only from the environment.
type Config struct {
	Addr       string
	Dir        string
	BaseURL    string
	AdminToken string
	Dev        bool
	Messenger  string

	WAPhoneID         string
	WAToken           string
	WATplLogin        string
	WATplAlert        string
	WALang            string
	RetainHistoryDays int    // price_history older than this is purged daily (default 90)
	RetainDiagDays    int    // check log and finished outbox rows older than this are purged daily (default 30)
	BackupKey         []byte // 32-byte AES-256-GCM key for backups; nil = unencrypted (still 0600)
	RespectRobots     bool   // WW_RESPECT_ROBOTS=1: opt-in robots.txt checks (default off)
	SendBudget        int    // global outbound alert messages per hour
	WAAppSecret       string // Meta webhook HMAC secret
	WAVerifyToken     string // Meta webhook GET verification token

	TwSID          string
	TwToken        string
	TwFrom         string
	TwContentLogin string
	TwContentAlert string
}

// LoadConfig builds a Config from an environment lookup function.
func LoadConfig(getenv func(string) string) (Config, error) {
	def := func(k, d string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return d
	}
	c := Config{
		Addr:           def("WW_ADDR", "127.0.0.1:8780"),
		Dir:            def("WW_DIR", "./data"),
		BaseURL:        strings.TrimRight(def("WW_BASE_URL", ""), "/"),
		AdminToken:     strings.TrimSpace(getenv("WW_ADMIN_TOKEN")),
		Dev:            getenv("WW_DEV") == "1",
		Messenger:      strings.ToLower(def("WW_MESSENGER", "log")),
		WAPhoneID:      strings.TrimSpace(getenv("WW_WA_PHONE_ID")),
		WAToken:        strings.TrimSpace(getenv("WW_WA_TOKEN")),
		WATplLogin:     def("WW_WA_TPL_LOGIN", "webwatcher_login"),
		WATplAlert:     def("WW_WA_TPL_ALERT", "webwatcher_alert"),
		WALang:         def("WW_WA_LANG", "en_GB"),
		WAAppSecret:    strings.TrimSpace(getenv("WW_WA_APP_SECRET")),
		WAVerifyToken:  strings.TrimSpace(getenv("WW_WA_VERIFY_TOKEN")),
		TwSID:          strings.TrimSpace(getenv("WW_TWILIO_SID")),
		TwToken:        strings.TrimSpace(getenv("WW_TWILIO_TOKEN")),
		TwFrom:         strings.TrimSpace(getenv("WW_TWILIO_FROM")),
		TwContentLogin: strings.TrimSpace(getenv("WW_TWILIO_CONTENT_LOGIN")),
		TwContentAlert: strings.TrimSpace(getenv("WW_TWILIO_CONTENT_ALERT")),
	}
	for _, d := range []struct {
		name string
		dst  *int
		def  int
	}{{"WW_RETAIN_HISTORY_DAYS", &c.RetainHistoryDays, 90}, {"WW_RETAIN_DIAG_DAYS", &c.RetainDiagDays, 30}} {
		*d.dst = d.def
		if v := strings.TrimSpace(getenv(d.name)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 3650 {
				return c, fmt.Errorf("%s must be a whole number of days from 1 to 3650, got %q", d.name, v)
			}
			*d.dst = n
		}
	}
	if v := strings.TrimSpace(getenv("WW_BACKUP_KEY")); v != "" {
		k, err := decodeBackupKey(v)
		if err != nil {
			return c, err
		}
		c.BackupKey = k
	}
	c.RespectRobots = getenv("WW_RESPECT_ROBOTS") == "1"
	c.SendBudget = 200
	if v := strings.TrimSpace(getenv("WW_SEND_BUDGET_PER_HOUR")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("WW_SEND_BUDGET_PER_HOUR must be a positive whole number, got %q", v)
		}
		c.SendBudget = n
	}
	if c.BaseURL == "" {
		c.BaseURL = "http://" + c.Addr
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("WW_BASE_URL must be an http(s) URL, got %q", c.BaseURL)
	}
	switch c.Messenger {
	case "log":
	case "meta":
		if c.WAPhoneID == "" || c.WAToken == "" {
			return c, errors.New("WW_MESSENGER=meta needs WW_WA_PHONE_ID and WW_WA_TOKEN")
		}
	case "twilio":
		if c.TwSID == "" || c.TwToken == "" || c.TwFrom == "" {
			return c, errors.New("WW_MESSENGER=twilio needs WW_TWILIO_SID, WW_TWILIO_TOKEN and WW_TWILIO_FROM")
		}
	default:
		return c, fmt.Errorf("unknown WW_MESSENGER %q (use log, meta or twilio)", c.Messenger)
	}
	return c, nil
}

func decodeBackupKey(v string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(v); err == nil && len(k) == 32 {
			return k, nil
		}
	}
	return nil, errors.New("WW_BACKUP_KEY must be 32 random bytes, base64 encoded (for example: openssl rand -base64 32)")
}

// SecureCookies reports whether session cookies should carry the Secure flag.
// They always do when the public URL is HTTPS; plain-HTTP is for local dev only.
func (c Config) SecureCookies() bool { return strings.HasPrefix(c.BaseURL, "https://") }
