package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Settings are what is saved between runs (never the token: that has its own file).
type Settings struct {
	Server string `json:"server"`
}

func tokenPath(d Dirs) string    { return filepath.Join(d.Config, "token") }
func settingsPath(d Dirs) string { return filepath.Join(d.Config, "config.json") }
func statusPath(d Dirs) string   { return filepath.Join(d.Config, "status.json") }

// LoadToken reads the saved token, or "" if there is none.
func LoadToken(d Dirs) (string, error) {
	b, err := os.ReadFile(tokenPath(d))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

// SaveToken writes the token to a file only the current user can read: mode
// 0600 and a 0700 directory on Unix, and on Windows an ACL that removes
// inherited access and grants the current user alone.
func SaveToken(d Dirs, token string) error {
	if err := os.MkdirAll(d.Config, 0o700); err != nil {
		return err
	}
	p := tokenPath(d)
	if err := os.WriteFile(p, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(p, 0o600); err != nil {
		return err
	}
	return restrictToUser(p)
}

// LoadSettings reads config.json; a missing file is empty settings.
func LoadSettings(d Dirs) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(settingsPath(d))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func SaveSettings(d Dirs, s Settings) error {
	if err := os.MkdirAll(d.Config, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	return os.WriteFile(settingsPath(d), b, 0o600)
}

// LastResult is the agent's note of its latest report. It holds a host and a
// status, never a URL.
type LastResult struct {
	At     time.Time `json:"at"`
	Host   string    `json:"host"`
	Status string    `json:"status"`
	Class  string    `json:"class,omitempty"`
}

func SaveLastResult(d Dirs, r LastResult) error {
	if err := os.MkdirAll(d.Config, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	return os.WriteFile(statusPath(d), b, 0o600)
}

func LoadLastResult(d Dirs) (LastResult, bool) {
	var r LastResult
	b, err := os.ReadFile(statusPath(d))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return r, false
	}
	return r, true
}
