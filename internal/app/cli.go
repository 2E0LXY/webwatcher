package app

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultEnvFile is the systemd EnvironmentFile the installer creates.
const DefaultEnvFile = "/etc/webwatcher.env"

// ParseEnvFile reads a systemd-style EnvironmentFile: KEY=VALUE lines, blank
// lines and lines starting with # or ; ignored, one pair of surrounding quotes
// stripped from values.
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

// EnvLookup returns the getenv function the CLI subcommands use. With an
// explicit file it reads only that file (as the service does); with an empty
// path it uses the process environment, falling back to DefaultEnvFile when
// WW_BASE_URL is not set there and that file exists.
func EnvLookup(file string, processEnv func(string) string) (func(string) string, error) {
	if file == "" && strings.TrimSpace(processEnv("WW_BASE_URL")) == "" {
		if _, err := os.Stat(DefaultEnvFile); err == nil {
			file = DefaultEnvFile
		}
	}
	if file == "" {
		return processEnv, nil
	}
	m, err := ParseEnvFile(file)
	if err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	return func(k string) string { return m[k] }, nil
}

// Doctor prints OK/WARN/FAIL lines describing whether webwatcher is ready to
// run with the given environment, and returns the number of FAIL lines. It
// prints variable names, never values of secrets.
func Doctor(getenv func(string) string, w io.Writer) int {
	fails := 0
	line := func(level, format string, a ...any) {
		if level == "FAIL" {
			fails++
		}
		fmt.Fprintf(w, "%-4s %s\n", level, fmt.Sprintf(format, a...))
	}
	set := func(k string) bool { return strings.TrimSpace(getenv(k)) != "" }
	// need reports each required variable by name and returns whether all are set.
	need := func(names ...string) bool {
		all := true
		for _, n := range names {
			if set(n) {
				line("OK", "%s is set", n)
			} else {
				line("FAIL", "%s is missing", n)
				all = false
			}
		}
		return all
	}

	dev := getenv("WW_DEV") == "1"
	base := strings.TrimRight(strings.TrimSpace(getenv("WW_BASE_URL")), "/")
	hostOK := false
	switch {
	case base == "":
		line("FAIL", "WW_BASE_URL is not set (use https://your.domain)")
	default:
		u, err := url.Parse(base)
		switch {
		case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
			line("FAIL", "WW_BASE_URL is not a valid http(s) URL")
		case u.Scheme != "https" && dev:
			line("WARN", "WW_BASE_URL is http (allowed only because WW_DEV=1)")
			hostOK = true
		case u.Scheme != "https":
			line("FAIL", "WW_BASE_URL must be https (plain http needs WW_DEV=1, development only)")
		case strings.HasSuffix(u.Hostname(), "example.com"):
			line("FAIL", "WW_BASE_URL still uses the example.com placeholder: set your real domain")
		default:
			line("OK", "WW_BASE_URL is https (%s)", base)
			hostOK = true
		}
	}

	msgr := strings.ToLower(strings.TrimSpace(getenv("WW_MESSENGER")))
	if msgr == "" {
		msgr = "log"
	}
	switch msgr {
	case "twilio":
		line("OK", "messenger: twilio")
		need("WW_TWILIO_SID", "WW_TWILIO_TOKEN", "WW_TWILIO_FROM")
		if set("WW_TWILIO_CONTENT_LOGIN") && set("WW_TWILIO_CONTENT_ALERT") {
			line("OK", "templates: Twilio content templates configured (WW_TWILIO_CONTENT_LOGIN, WW_TWILIO_CONTENT_ALERT)")
		} else {
			line("WARN", "templates: no Twilio content template SIDs; free-form text is used, which the sandbox only delivers within 24 hours of the user's last message")
		}
	case "meta":
		line("OK", "messenger: meta")
		need("WW_WA_PHONE_ID", "WW_WA_TOKEN")
		tl, ta, lang := orDefault(getenv("WW_WA_TPL_LOGIN"), "webwatcher_login"), orDefault(getenv("WW_WA_TPL_ALERT"), "webwatcher_alert"), orDefault(getenv("WW_WA_LANG"), "en_GB")
		line("OK", "templates: login=%s alert=%s language=%s (must exist and be approved in WhatsApp Manager)", tl, ta, lang)
	case "log":
		if dev {
			line("WARN", "messenger: log (development only; login links are written to the log)")
		} else {
			line("FAIL", "WW_MESSENGER is not set to twilio or meta (the log messenger needs WW_DEV=1 and is development only)")
		}
	default:
		line("FAIL", "WW_MESSENGER has an unknown value (use twilio or meta)")
	}

	// Webhook URLs to paste into the provider consoles.
	if hostOK {
		switch msgr {
		case "twilio":
			if set("WW_TWILIO_TOKEN") {
				line("OK", "Twilio webhook (sandbox: 'When a message comes in', method POST): %s/hook/twilio", base)
			} else {
				line("FAIL", "Twilio webhook %s/hook/twilio is disabled until WW_TWILIO_TOKEN is set", base)
			}
		case "meta":
			if set("WW_WA_APP_SECRET") && set("WW_WA_VERIFY_TOKEN") {
				line("OK", "Meta webhook callback URL: %s/hook/meta (verify token: the value of WW_WA_VERIFY_TOKEN)", base)
			} else {
				line("FAIL", "Meta webhook %s/hook/meta is disabled until WW_WA_APP_SECRET and WW_WA_VERIFY_TOKEN are set (STOP replies would not be processed)", base)
			}
		}
	}

	// Data directory.
	dir := orDefault(getenv("WW_DIR"), "./data")
	checkDir(dir, line)

	if set("WW_ADMIN_TOKEN") {
		line("OK", "WW_ADMIN_TOKEN is set (/healthz enabled)")
	} else {
		line("WARN", "WW_ADMIN_TOKEN is not set (/healthz is disabled)")
	}
	if set("WW_BACKUP_KEY") {
		if _, err := decodeBackupKey(strings.TrimSpace(getenv("WW_BACKUP_KEY"))); err != nil {
			line("FAIL", "WW_BACKUP_KEY is not 32 random bytes in base64 (openssl rand -base64 32)")
		} else {
			line("OK", "WW_BACKUP_KEY is valid (backups encrypted)")
		}
	} else {
		line("WARN", "WW_BACKUP_KEY is not set (backups are written unencrypted, mode 0600)")
	}

	// The authoritative check: exactly what the service does at start-up.
	if _, err := LoadConfig(getenv); err != nil {
		line("FAIL", "the service would NOT start: %s", err)
	} else {
		line("OK", "the service would start (configuration accepted)")
	}
	if fails == 0 {
		fmt.Fprintln(w, "All checks passed.")
	} else {
		fmt.Fprintf(w, "%d check(s) failed.\n", fails)
	}
	return fails
}

func orDefault(v, d string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return d
}

func checkDir(dir string, line func(level, format string, a ...any)) {
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		line("WARN", "data directory %s does not exist yet (created 0700 on first start)", dir)
		return
	case err != nil:
		line("FAIL", "cannot read data directory %s: %v", dir, err)
		return
	case !fi.IsDir():
		line("FAIL", "%s is not a directory", dir)
		return
	}
	if fi.Mode().Perm()&0o077 != 0 {
		line("FAIL", "data directory %s has mode %04o; it must be 0700 (chmod 700 %s)", dir, fi.Mode().Perm(), dir)
	} else {
		line("OK", "data directory %s has mode %04o", dir, fi.Mode().Perm())
	}
	db := filepath.Join(dir, "webwatcher.db")
	if st, err := os.Stat(db); err == nil && st.Mode().Perm()&0o077 != 0 {
		line("FAIL", "%s has mode %04o; it must be 0600", db, st.Mode().Perm())
	}
}

// AddUser creates (or re-uses) the user for phone and prints a one-time
// magic link to w. It talks to the database directly and is not reachable
// over HTTP. Only WW_DIR and WW_BASE_URL are read.
func AddUser(getenv func(string) string, phone string, w io.Writer) error {
	p, err := NormalisePhone(phone)
	if err != nil {
		return err
	}
	base := strings.TrimRight(strings.TrimSpace(getenv("WW_BASE_URL")), "/")
	u, err := url.Parse(base)
	if base == "" || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("WW_BASE_URL must be set to the public URL, for example https://watch.example.com")
	}
	if os.Geteuid() == 0 && os.Getenv("WW_ALLOW_ROOT") != "1" {
		return errors.New("do not run adduser as root: it would create database files the service cannot open. Use: sudo -u webwatcher webwatcher adduser " + p)
	}
	dir := orDefault(getenv("WW_DIR"), "./data")
	db, err := OpenDB(dir)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	st := NewStore(db, SystemClock())
	raw, hash := newToken()
	if err := st.CreateAdminLoginToken(p, hash, LoginTokenTTL); err != nil {
		return err
	}
	fmt.Fprintf(w, "One-time login link for %s (valid 15 minutes, works once; consent recorded as %q):\n\n%s/auth/%s\n\n", MaskPhone(p), ConsentAdminCreated, base, raw)
	if u.Scheme != "https" {
		fmt.Fprintln(w, "Note: WW_BASE_URL is not https; production requires https.")
	}
	return nil
}
