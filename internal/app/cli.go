package app

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	case "dashboard":
		line("OK", "messenger: dashboard (no WhatsApp provider needed)")
		line("WARN", "alerts are visible only inside the dashboard until a WhatsApp provider is configured")
	case "log":
		if dev {
			line("WARN", "messenger: log (development only; login links are written to the log)")
		} else {
			line("FAIL", "WW_MESSENGER is not set to twilio or meta (the log messenger needs WW_DEV=1 and is development only)")
		}
	default:
		line("FAIL", "WW_MESSENGER has an unknown value (use dashboard, twilio or meta)")
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

// geteuid is replaced in tests to exercise the root refusal.
var geteuid = os.Geteuid

// cliEnv is the shared set-up of the operator commands that touch the database.
type cliEnv struct {
	db    *sql.DB
	store *Store
	phone string
	base  string
	https bool
	dash  bool
}

func (c *cliEnv) Close() { c.db.Close() }

// openCLI validates the number and WW_BASE_URL, refuses to run as root (it would
// create database files the service cannot open) and opens the database.
func openCLI(getenv func(string) string, cmd, phone string, needBase bool) (*cliEnv, error) {
	p, err := NormalisePhone(phone)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(strings.TrimSpace(getenv("WW_BASE_URL")), "/")
	u, err := url.Parse(base)
	if needBase && (base == "" || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https")) {
		return nil, errors.New("WW_BASE_URL must be set to the public URL, for example https://watch.example.com")
	}
	if geteuid() == 0 && os.Getenv("WW_ALLOW_ROOT") != "1" {
		return nil, errors.New("do not run " + cmd + " as root: it would create database files the service cannot open. Use: sudo -u webwatcher webwatcher " + cmd + " " + p)
	}
	db, err := OpenDB(orDefault(getenv("WW_DIR"), "./data"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	c := &cliEnv{db: db, store: NewStore(db, SystemClock()), phone: p, base: base, dash: strings.ToLower(strings.TrimSpace(getenv("WW_MESSENGER"))) == "dashboard"}
	c.https = u != nil && u.Scheme == "https"
	return c, nil
}

// AddUser creates (or re-uses) the user for phone and prints a one-time
// magic link to w. It talks to the database directly and is not reachable
// over HTTP. Only WW_DIR, WW_BASE_URL and WW_MESSENGER are read.
func AddUser(getenv func(string) string, phone string, w io.Writer) error {
	return issueLink(getenv, phone, w, false, false)
}

// AddUserAdmin is AddUser with --admin: the account is also marked admin.
func AddUserAdmin(getenv func(string) string, phone string, w io.Writer) error {
	return issueLink(getenv, phone, w, false, true)
}

// LoginLink prints a fresh one-time login link for an existing user. It is
// adduser for a number that must already have an account.
func LoginLink(getenv func(string) string, phone string, w io.Writer) error {
	return issueLink(getenv, phone, w, true, false)
}

func issueLink(getenv func(string) string, phone string, w io.Writer, mustExist, admin bool) error {
	cmd := "adduser"
	if mustExist {
		cmd = "loginlink"
	}
	c, err := openCLI(getenv, cmd, phone, true)
	if err != nil {
		return err
	}
	defer c.Close()
	p, st := c.phone, c.store
	if mustExist {
		ok, err := st.UserExists(p)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no user with number %s; create the account with: webwatcher adduser %s", MaskPhone(p), p)
		}
	}
	if admin {
		uid, err := st.EnsureUser(p, c.dash)
		if err != nil {
			return err
		}
		if err := st.SetAdmin(uid, true); err != nil {
			return err
		}
	}
	raw, hash := newToken()
	consent := fmt.Sprintf("consent recorded as %q", ConsentAdminCreated)
	if c.dash {
		err = st.CreateDashboardLoginToken(p, hash, LoginTokenTTL)
		consent = fmt.Sprintf("source %q, no WhatsApp consent recorded", ConsentDashboard)
	} else {
		err = st.CreateAdminLoginToken(p, hash, LoginTokenTTL)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "One-time login link for %s (valid 15 minutes, works once; %s):\n\n%s/auth/%s\n\n", MaskPhone(p), consent, c.base, raw)
	if admin {
		fmt.Fprintln(w, "This account is an admin.")
	}
	if !c.https {
		fmt.Fprintln(w, "Note: WW_BASE_URL is not https; production requires https.")
	}
	return nil
}

// PermalinkOpts selects what `webwatcher permalink` does.
type PermalinkOpts struct {
	Revoke bool // revoke the permanent link
	Status bool // print created/last used, never the token
	Admin  bool // also mark the account admin (create/rotate only)
}

// Permalink creates or rotates the permanent personal login link for a number
// (creating the account if absent), revokes it, or prints its status. The token
// is printed once, on creation, and only its hash is stored.
func Permalink(getenv func(string) string, phone string, o PermalinkOpts, w io.Writer) error {
	if o.Revoke && o.Status {
		return errors.New("use only one of --revoke and --show-status")
	}
	c, err := openCLI(getenv, "permalink", phone, !o.Revoke && !o.Status)
	if err != nil {
		return err
	}
	defer c.Close()
	st, p := c.store, c.phone
	switch {
	case o.Revoke, o.Status:
		uid, err := st.UserID(p)
		if err != nil {
			return fmt.Errorf("no user with number %s", MaskPhone(p))
		}
		if o.Revoke {
			ok, err := st.RevokePersonalLink(uid)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%s has no active permanent link", MaskPhone(p))
			}
			fmt.Fprintf(w, "Permanent link for %s revoked.\n", MaskPhone(p))
			return nil
		}
		info, err := st.PersonalLinkStatus(uid)
		if err != nil {
			return err
		}
		stamp := func(t time.Time) string { return t.In(london).Format("2 Jan 2006 15:04 MST") }
		switch {
		case info.CreatedAt.IsZero():
			fmt.Fprintf(w, "%s: no permanent link.\n", MaskPhone(p))
		case info.RevokedAt != nil:
			fmt.Fprintf(w, "%s: revoked %s (created %s).\n", MaskPhone(p), stamp(*info.RevokedAt), stamp(info.CreatedAt))
		default:
			last := "never"
			if info.LastUsedAt != nil {
				last = stamp(*info.LastUsedAt)
			}
			fmt.Fprintf(w, "%s: active. Created %s. Last used %s.\n", MaskPhone(p), stamp(info.CreatedAt), last)
		}
		return nil
	}
	uid, err := st.EnsureUser(p, c.dash)
	if err != nil {
		return err
	}
	if o.Admin {
		if err := st.SetAdmin(uid, true); err != nil {
			return err
		}
	}
	raw, hash := newToken()
	if err := st.SetPersonalLink(uid, hash); err != nil {
		return err
	}
	fmt.Fprintf(w, "Permanent login link for %s. It is shown once and cannot be recovered; running this command again replaces it.\nAnyone holding it has full access to the account: treat it like a password.\n\n%s/p/%s\n\n", MaskPhone(p), c.base, raw)
	if o.Admin {
		fmt.Fprintln(w, "This account is an admin.")
	}
	if !c.https {
		fmt.Fprintln(w, "Note: WW_BASE_URL is not https; production requires https.")
	}
	return nil
}

// SetAdmin marks (or with revoke, unmarks) an account as admin, creating the
// account when absent. Removing the last enabled admin is refused.
func SetAdmin(getenv func(string) string, phone string, revoke bool, w io.Writer) error {
	c, err := openCLI(getenv, "setadmin", phone, false)
	if err != nil {
		return err
	}
	defer c.Close()
	if revoke {
		uid, err := c.store.UserID(c.phone)
		if err != nil {
			return fmt.Errorf("no user with number %s", MaskPhone(c.phone))
		}
		if err := c.store.SetAdmin(uid, false); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s is no longer an admin.\n", MaskPhone(c.phone))
		return nil
	}
	uid, err := c.store.EnsureUser(c.phone, c.dash)
	if err != nil {
		return err
	}
	if err := c.store.SetAdmin(uid, true); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s is an admin.\n", MaskPhone(c.phone))
	return nil
}
