# webwatcher

A small self-hosted price watcher. Paste the URL of a product, holiday or flight page, and webwatcher polls it. When the price changes it sends you a WhatsApp message, and if you set a target price you get a separate "below target" alert.

One static Go binary (Go standard library plus `modernc.org/sqlite` and `golang.org/x/net/html`), SQLite storage, server-rendered HTML with plain CSS and no JavaScript framework (one optional 20-line script refreshes the unread badge). It listens on `127.0.0.1` and is meant to sit behind Caddy.

## Quick start (10 minutes)

For personal use first: you and a few friends, on a Debian VPS with Caddy installed and a domain pointing at it (replace `watch.example.com` throughout).

**1. Install.** Releases are published as pre-releases by CI; the installer finds the newest one, checks its SHA-256 and sets everything up. Preview first with `--dry-run`.

```sh
curl -fsSLO https://raw.githubusercontent.com/2E0LXY/webwatcher/main/deploy/install.sh
sudo sh install.sh --domain watch.example.com --dry-run   # prints what it would do
sudo sh install.sh --domain watch.example.com
```

If the repository is private, `export GITHUB_TOKEN=...` first, or build it yourself and add `--binary ./webwatcher`. It creates the `webwatcher` user and `/var/lib/webwatcher` (0700), writes `/etc/webwatcher.env` only if it does not exist (with a generated `WW_ADMIN_TOKEN` and your base URL), adds a validated Caddy site (after backing up the Caddyfile) and enables the service. It never asks for provider secrets. The service will not start until step 2 is done; that is deliberate.

### Dashboard-only mode

No WhatsApp provider is needed to start. Alerts appear inside the web dashboard (an unread badge in the header, a banner on the dashboard and a **Notifications** page) until you add a provider later.

**2. Configure.** Edit `/etc/webwatcher.env` (`sudo nano /etc/webwatcher.env`):

```sh
WW_BASE_URL=https://watch.example.com
WW_MESSENGER=dashboard
```

No provider variables are required. Then check and start:

```sh
sudo webwatcher doctor          # prints OK, plus a WARN that alerts are visible only inside the dashboard
sudo systemctl restart webwatcher
journalctl -u webwatcher -n 20
```

`doctor` prints variable names only, never secret values.

**3. Log in.** The login page tells visitors to ask the administrator, and gives the same answer for every number. You create links yourself, as the service user (not root, or the database would be created unreadable by the service):

```sh
sudo -u webwatcher webwatcher adduser +447700900123      # new or existing user
sudo -u webwatcher webwatcher loginlink +447700900123    # existing user only; same output
```

Each prints a one-time link (valid 15 minutes, single use, stored only as a hash) to your terminal. Send it to the person; they open it, press the confirm button and are in. Under the dashboard messenger the user is recorded with source `dashboard` and **no** WhatsApp consent: nothing is sent to their phone, and if you switch to WhatsApp later they receive nothing until they have ticked the WhatsApp opt-in. The commands talk to the database directly; there is no web equivalent.

**4. Add a first watch and a target.** On the dashboard, under **Add a page to watch**, paste a single product or package page URL, optionally a label, enter a **target price** in pounds (you get a separate "below target" alert when the price falls to it or lower) and choose how often to check (1 hour is the default). Press **Start watching**; the first check happens shortly after and the row shows the last price and status. You can change the target later from the watch's page. When the price changes or reaches the target, a notification appears. The newest 200 per user are kept, and ones older than `WW_RETAIN_HISTORY_DAYS` (default 90) are removed.

The unread badge refreshes itself every minute through `GET /notifications/count` (plain text) using a 20-line same-origin script, `/static/notify.js`; the content security policy is unchanged and there is no inline script. Without JavaScript the badge updates when you load a page.

### Later: WhatsApp alerts

Switch when you have a provider: set `WW_MESSENGER=twilio` or `meta` as below, run `webwatcher doctor`, restart. Existing notifications stay in the dashboard. Users created in dashboard mode have no WhatsApp consent, so each needs to log in through the WhatsApp login form and tick the opt-in (or you run `adduser` for them, which records `admin-created`).

**Choose a WhatsApp path and edit `/etc/webwatcher.env`** (`sudo nano /etc/webwatcher.env`; uncomment one block).

*Path A: Twilio WhatsApp sandbox (no template approval)*

```sh
WW_BASE_URL=https://watch.example.com
WW_MESSENGER=twilio
WW_TWILIO_SID=ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
WW_TWILIO_TOKEN=your-twilio-auth-token
WW_TWILIO_FROM=+14155238886
```

- In the Twilio console open Messaging, Try it out, Send a WhatsApp message. Use the sandbox number shown there as `WW_TWILIO_FROM`.
- Each person (you and each friend) sends the sandbox's `join <two-words>` code to that number from their own WhatsApp, once.
- In Sandbox settings set **When a message comes in** to `https://watch.example.com/hook/twilio`, method `POST` (this makes STOP work).
- Sandbox limits to know about: free-form messages only reach someone who has messaged the sandbox in the last 24 hours, and sandbox membership expires after a few days, so people re-send the join code now and then. It is fine for a handful of people, not for production.

*Path B: Meta WhatsApp Cloud API (production)*

```sh
WW_BASE_URL=https://watch.example.com
WW_MESSENGER=meta
WW_WA_PHONE_ID=123456789012345
WW_WA_TOKEN=your-permanent-access-token
WW_WA_APP_SECRET=your-app-secret
WW_WA_VERIFY_TOKEN=any-long-random-string-you-choose
```

- Create the two Utility templates `webwatcher_login` and `webwatcher_alert` and wait for approval (text in [Option A](#option-a-meta-whatsapp-cloud-api-ww_messengermeta) below).
- In the Meta app's WhatsApp configuration set the callback URL to `https://watch.example.com/hook/meta` with your verify token, and subscribe to `messages`.
- Until the templates are approved you can still log in with `adduser` (step 4), but alerts to numbers outside a 24-hour chat window will not be delivered.

**Check it.**

```sh
sudo webwatcher doctor          # every line should be OK or WARN; it also prints the webhook URLs to paste
sudo systemctl restart webwatcher
journalctl -u webwatcher -n 20
```

`doctor` prints variable names only, never secret values.

**Log in without WhatsApp.** Run this as the service user (not root, or the database would be created unreadable by the service):

```sh
sudo -u webwatcher webwatcher adduser +447700900123
```

It prints a one-time link (valid 15 minutes, single use, stored only as a hash) straight to your terminal; open it, press the confirm button, and you are in. The user is recorded with consent `admin-created`, so only add people who have agreed to receive messages from you. They can send STOP at any time. The command talks to the database directly; there is no web equivalent. Friends can use the normal login form (it sends the link over WhatsApp) once your provider is working for them, or you can run `adduser` for each of them and send them the link yourself.


**Honest note.** Webwatcher fetches pages like a plain HTTP client and does not evade bot protection. Many travel and retail sites (Jet2, TUI, easyJet and others) will answer with a block or a JavaScript-only page. When that happens the watch shows as **blocked** or **needs browser rendering**; it is reported, not bypassed, and there is nothing to configure that will change it. Try a different page for the same product, a CSS selector, or accept that the site cannot be watched.

Upgrading later: re-run `sudo sh install.sh --domain watch.example.com`. It is idempotent and leaves `/etc/webwatcher.env` alone.

## What it does

- Login by magic link sent over WhatsApp. No passwords. Single-use links that expire after 15 minutes; sessions last 30 days. The login form has a required opt-in tick ("I agree to receive WhatsApp messages from Webwatcher about my watches"), recorded with a timestamp. **Log out everywhere** on the Account page revokes every session.
- Consent and opt-out: no alert is ever sent without a recorded opt-in. Alerts can be switched off on the dashboard, and replying STOP (or UNSUBSCRIBE) to a message does the same through the provider webhook; START turns them back on. Prices keep being tracked while alerts are off.
- Dashboard: add a URL (optional label, CSS selector, target price, check interval), see last price, last checked time and status, pause, resume, edit target, delete, and view price history. Up to 30 watches per user.
- Price extraction, in order: your CSS selector, then JSON-LD `Product`/`Offer`, then `itemprop`/`og:price:amount`/`product:price:amount` meta tags, then a heuristic search of the page text for `£` amounts (preferring "from £N" style prices and ignoring "was", "save" and "deposit" amounts).
- Check intervals: 30 minutes, 1 hour (default), 3 hours, 6 hours or 24 hours, with jitter and exponential backoff on errors. After 10 failures in a row the watch stops and you are told once.
- Alerts fire on transitions: price changed by at least 1p ("changed"), and price crossing from above the target to at or below it ("below target"). A later recovery above the target re-arms the below-target alert, and so does editing the target. One price move sends one message (below target takes priority over changed), and an unchanged price never repeats an alert.
- Alerts are written to a transactional outbox in the same database transaction as the price update, then delivered by a sender loop with up to 5 attempts and exponential backoff. They survive restarts. Immediately before each individual message (not once per batch) the watch and owner are re-checked: if the watch was deleted, paused or had its target edited, or the owner opted out, was disabled or lacks consent for the current messenger, the alert is dropped, so an opt-out or deletion during a batch stops the rows still waiting. A message already handed to the provider cannot be recalled. A global budget (`WW_SEND_BUDGET_PER_HOUR`, default 200) caps outbound messages per hour; login links may use at most half of it so they cannot starve alerts.
- Price basis: every observation is stored with its currency and, when detectable from JSON-LD or the wording ("per person", "pp", "total", "for 2 adults"), its basis. Observations with a different currency or basis are never compared: the watch is flagged **Basis changed** on the dashboard, no alert is sent, and you confirm with a button that resets the baseline.
- Target currency: each watch stores the currency its target is in (GBP by default; GBP, EUR or USD can be chosen). If the page shows a different currency the price is recorded but never compared with the target, so no below-target alert fires, and the watch is flagged **Currency mismatch** on the dashboard until the currencies agree.
- Sold-out pages (JSON-LD availability) keep the last valid price and send nothing. A page that loads but has no readable price is reported as **No price found**; a network error or 5xx is a **Temporary error**; both keep the last valid price.
- The fetcher uses conditional GET (ETag / Last-Modified), honours `Retry-After` on 429 and 503, rejects truncated or over-size pages instead of parsing them, and applies the per-host rate budget to redirect destinations as well.

## Honest limitations

- **Plain HTTP only.** Webwatcher fetches the page the way a simple HTTP client does. It does not run JavaScript. Many travel sites, including **Jet2, Jet2holidays, TUI and easyJet**, build prices in the browser and/or sit behind bot protection (Akamai, Cloudflare and similar). Expect some of those pages to fail.
- **Bot protection is respected, not evaded.** If a site answers 403/429 or serves a bot-challenge page, the watch is marked `blocked` and the dashboard tells you so. Webwatcher uses an honest `webwatcher/<version>` User-Agent and will never spoof browsers, rotate proxies or use stealth tricks. A blocked page is reported, not bypassed, and cannot be watched.
- **robots.txt is not consulted by default.** You, as the operator, are responsible for complying with each site's terms of use and for deciding which pages are appropriate to poll at a personal, low rate. Check each site's current terms before watching it. If you want Webwatcher to check robots.txt, set `WW_RESPECT_ROBOTS=1`: it then fetches `/robots.txt` once per host (cached 24 hours), does not fetch pages that `Disallow` matches for the `webwatcher` user agent, and shows the watch as **Disallowed by robots.txt**. An unreachable robots.txt counts as allowed.
- **Pages that need JavaScript** show "needs browser rendering". Check with "View source" in your browser: if the price is not in the source text, plain fetching cannot see it. A headless-browser fetcher is planned as a later addition; the fetcher is an interface so it can be added without touching the rest.
- A listing page with many prices is ambiguous. Use a single product or package page, or give a CSS selector.
- If you set a selector and it stops matching (the site changed), the watch fails with "selector matched nothing". It never silently falls back to another method, so a redesign cannot cause a bogus price alert.
- Politeness: at most 4 fetches at once, at least 60 seconds between requests to the same host, 2 MB body cap, 20 second timeout, 5 redirects.

### Using the CSS selector override

1. Open the page in your browser, right-click the price, choose **Inspect**.
2. Note a class, id or attribute on the element or a parent, for example `<span class="price-box__amount">`.
3. Enter a selector such as `.price-box__amount`, `#total-price`, `div.summary > span.price` or `[data-price]`.

Supported syntax: tag, `*`, `.class`, `#id`, `[attr]`, `[attr=value]`, `[attr^=value]`, `[attr$=value]`, `[attr*=value]`, `[attr~=value]`, compounds such as `span.price[data-x]`, descendant (space) and child (`>`) combinators. No commas, pseudo-classes or sibling combinators. The text (or a `content`/`data-price` attribute) of the first match is searched for an amount.

## Installation

The quick start above uses `deploy/install.sh`. The manual equivalent is below.

Requires a Linux host, Caddy, and a WhatsApp sender (see below). Use a pre-built release binary, or build with Go 1.26:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(cat VERSION)" -o webwatcher .
```

```sh
sudo useradd --system --home /var/lib/webwatcher --shell /usr/sbin/nologin webwatcher
sudo install -m 0755 webwatcher /usr/local/bin/webwatcher
sudo install -m 0640 -o root -g webwatcher /dev/null /etc/webwatcher.env   # then edit it, see below
sudo cp deploy/webwatcher.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now webwatcher
```

Add `deploy/Caddyfile.webwatcher` to your Caddy configuration (change the host name) and reload Caddy. It proxies to `127.0.0.1:8780`, sets security headers and does not write `/auth/` URLs to the access log.

Example `/etc/webwatcher.env`:

```sh
WW_DIR=/var/lib/webwatcher
WW_BASE_URL=https://watch.example.com
WW_MESSENGER=meta
WW_WA_PHONE_ID=123456789012345
WW_WA_TOKEN=...
WW_WA_APP_SECRET=...
WW_WA_VERIFY_TOKEN=a-long-random-string
WW_BACKUP_KEY=$(openssl rand -base64 32)   # paste the generated value
WW_ADMIN_TOKEN=some-long-random-string
```

Data lives in `WW_DIR` (mode 0700): `webwatcher.db` (SQLite, WAL mode, mode 0600) and `backups/` (mode 0700): a daily snapshot, AES-256-GCM encrypted when `WW_BACKUP_KEY` is set, mode 0600, deleted after 7 days. To restore an encrypted backup: `WW_BACKUP_KEY=... webwatcher -decrypt-backup backups/webwatcher-YYYYMMDD.db.enc -out restored.db`.

Webwatcher refuses to start unless it is configured for production: `WW_BASE_URL` must be `https://` and `WW_MESSENGER` must be `dashboard`, `meta` or `twilio`. Only with `WW_DEV=1` are an `http://` base URL and the `log` messenger allowed.

### Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `WW_ADDR` | `127.0.0.1:8780` | Listen address. Keep it on loopback behind Caddy. |
| `WW_DIR` | `./data` | Data directory (database and backups). |
| `WW_BASE_URL` | `http://<WW_ADDR>` | Public URL, used in login links, alerts and Twilio signature checks. Must be `https://` unless `WW_DEV=1`; cookies get the `Secure` flag (and `__Host-` prefix) when it is HTTPS. |
| `WW_ADMIN_TOKEN` | empty | Bearer token for `/healthz`. If empty, `/healthz` returns 404. |
| `WW_MESSENGER` | `log` | `dashboard`, `meta` or `twilio` (or `log`). `dashboard` needs no provider variables: alerts are stored as in-dashboard notifications. `log` is refused unless `WW_DEV=1` and the base URL is http. |
| `WW_DEV` | unset | `1` allows an http base URL and the `log` messenger, and shows `/dev` with captured login links. Never use in production. |
| `WW_SEND_BUDGET_PER_HOUR` | `200` | Global cap on outbound WhatsApp messages per rolling hour (alerts and login links). Over-budget alerts stay queued. |
| `WW_RETAIN_HISTORY_DAYS` | `90` | Price history older than this is purged daily. |
| `WW_RETAIN_DIAG_DAYS` | `30` | Check logs and finished outbox rows older than this are purged daily. |
| `WW_BACKUP_KEY` | empty | 32 random bytes, base64 (`openssl rand -base64 32`). If set, backups are AES-256-GCM encrypted. If empty, backups are plaintext (still mode 0600) and a warning is logged. |
| `WW_SESSION_DAYS` | `30` | Login session lifetime in days (1 to 365). |
| `WW_MAX_USERS` | `25` | Account cap for adding accounts from the admin page (the command line is not capped). |
| `WW_RESPECT_ROBOTS` | unset | `1` makes the fetcher check robots.txt (opt-in, see limitations). |
| `WW_WA_APP_SECRET` | | Meta: app secret, used to verify `X-Hub-Signature-256` on `POST /hook/meta`. Without it that endpoint is 404. |
| `WW_WA_VERIFY_TOKEN` | | Meta: token you choose, checked on the `GET /hook/meta` subscription challenge. Without it that endpoint is 404. |
| `WW_WA_PHONE_ID` | | Meta: WhatsApp Business phone number ID. |
| `WW_WA_TOKEN` | | Meta: permanent access token. |
| `WW_WA_TPL_LOGIN` | `webwatcher_login` | Meta: name of the approved login template. |
| `WW_WA_TPL_ALERT` | `webwatcher_alert` | Meta: name of the approved alert template. |
| `WW_WA_LANG` | `en_GB` | Meta: template language code. |
| `WW_TWILIO_SID` | | Twilio account SID. |
| `WW_TWILIO_TOKEN` | | Twilio auth token. Also verifies `X-Twilio-Signature` on `POST /hook/twilio`; with it unset that endpoint is 404. |
| `WW_TWILIO_FROM` | | Twilio WhatsApp sender, e.g. `+14155238886` (the `whatsapp:` prefix is added if missing). |
| `WW_TWILIO_CONTENT_LOGIN` | empty | Twilio: Content Template SID (`HX...`) for login messages. Empty sends free-form text. |
| `WW_TWILIO_CONTENT_ALERT` | empty | Twilio: Content Template SID for alerts. Empty sends free-form text. |

Secrets are read from the environment only. They are never stored in the database or written to logs.

The `log` messenger prints every message, including login links, to the log. It is for development only.

## Permanent login link

One-time links last 15 minutes and work once. For your own account you can instead have one **permanent link** that you bookmark. It is created only from the command line:

```
sudo -u webwatcher webwatcher permalink +447700900123              # create, or rotate (replaces the old link)
sudo -u webwatcher webwatcher permalink --show-status +447700900123 # created / last used, never the token
sudo -u webwatcher webwatcher permalink --revoke +447700900123      # revoke
```

The command prints `https://<your host>/p/<token>` once. Only a SHA-256 hash is stored, so it cannot be shown again; run the command again to replace it. The account is created if it does not exist. Opening the link starts a normal session (same cookie flags, `WW_SESSION_DAYS`) and redirects to the dashboard. Opening it does not use it up, so link-preview bots cannot burn it. A wrong token returns the same 404 as any unknown path, and an IP that fails 20 times in an hour gets 429.

**Security note: anyone holding the link has full access to the account. Treat it like a password.** Do not paste it into chats or shared notes. If it leaks, rotate or revoke it with the command above, or press Revoke under "Personal login link" on the dashboard. A hijacked session can revoke but never create a permanent link. Revoking does not end sessions already started: use Account, Log out everywhere as well. Deleting the account removes the link. The `/p/` path is never logged by Webwatcher, and `deploy/Caddyfile.webwatcher` keeps it out of Caddy's access log (`log_skip` covers `/auth/*` and `/p/*`).

## Adding accounts

Admins get an **Accounts** page (`/admin/users`, 404 for everyone else). Mark an account admin from the command line:

```
sudo -u webwatcher webwatcher setadmin +447700900123            # creates the account if absent
sudo -u webwatcher webwatcher setadmin --revoke +447700900123   # refused for the last enabled admin
sudo -u webwatcher webwatcher permalink --admin +447700900123   # also works with adduser --admin
```

From the page an admin can add an account (number normalised to `+44...`, optional display name, up to `WW_MAX_USERS`), create or rotate that account's permanent link (shown once, copy it at once, never logged), revoke it, disable or enable the account, and delete it (with a confirmation tick, erasing everything as Delete account does). Disabling ends the account's sessions, blocks both kinds of login link and makes the scheduler skip its watches. Admins cannot disable or delete themselves or the last enabled admin, and cannot create or rotate their own permanent link from the page (command line only). Every action is written to an audit log (who, action, account, time; no secrets), shown at the bottom of the page. Numbers on the page are masked. Normal users never see other accounts.

## WhatsApp setup

WhatsApp only lets a business start a conversation with a template message, so both providers need approved templates for login links and alerts (alerts are usually sent outside the 24-hour customer-service window).

### Option A: Meta WhatsApp Cloud API (`WW_MESSENGER=meta`)

1. In Meta Business Manager, create a Meta developer app with the WhatsApp product, and add a WhatsApp Business phone number.
2. Create a System User with a permanent access token that has the `whatsapp_business_messaging` permission. That is `WW_WA_TOKEN`. Copy the phone number ID as `WW_WA_PHONE_ID`.
3. In WhatsApp Manager, create two message templates, category **Utility**, language **English (UK)** (`en_GB`), names exactly as below (or set `WW_WA_TPL_LOGIN` / `WW_WA_TPL_ALERT`). Webwatcher sends only **body** parameters.

**`webwatcher_login`** (1 parameter)

```text
Your Webwatcher login link: {{1}} It works once and expires in 15 minutes. If you did not ask for it, ignore this message.
```

| Parameter | Content | Example for approval |
|---|---|---|
| `{{1}}` | Full magic-link URL | `https://watch.example.com/auth/AbCdEf123` |

**`webwatcher_alert`** (4 parameters)

```text
Webwatcher: {{1}}: {{2}} Page: {{3}} Dashboard: {{4}} Manage your watches on the dashboard.
```

| Parameter | Content | Example for approval |
|---|---|---|
| `{{1}}` | Watch label (or the site's host name) | `Majorca week in July` |
| `{{2}}` | What happened | `price changed £1,200.00 → £1,100.00` or `below target! Now £990.00 (target £1,000.00), was £1,200.00` |
| `{{3}}` | Page URL | `https://www.jet2holidays.com/...` |
| `{{4}}` | Dashboard URL | `https://watch.example.com/` |

Parameters never contain newlines, tabs or runs of spaces, as Meta requires. If Meta rejects URLs inside body variables for your account, adjust the template wording in Business Manager and keep the parameter order.

The request sent is `POST https://graph.facebook.com/v21.0/{WW_WA_PHONE_ID}/messages` with `Authorization: Bearer {WW_WA_TOKEN}`.

### Option B: Twilio (`WW_MESSENGER=twilio`)

1. Create a Twilio account. For testing use the WhatsApp Sandbox (each recipient must first send the join code to the sandbox number). For real use, register a WhatsApp sender.
2. Set `WW_TWILIO_SID`, `WW_TWILIO_TOKEN` and `WW_TWILIO_FROM`.
3. In the sandbox, or inside the 24-hour session window, free-form text works and nothing else is needed. For production create two Content Templates (type WhatsApp, with the same text and variables `{{1}}`..`{{4}}` as above), get them approved, and set `WW_TWILIO_CONTENT_LOGIN` and `WW_TWILIO_CONTENT_ALERT` to their SIDs. Webwatcher then sends `ContentSid` and `ContentVariables` (`{"1":"...","2":"..."}`).

## Inbound webhooks (STOP / START)

Both webhooks handle the keywords STOP, STOPALL, UNSUBSCRIBE, CANCEL, END and QUIT (alerts off) and START and UNSTOP (alerts on; only for numbers that have a recorded opt-in). Every request is verified with the provider's signature, compared in constant time, limited to 256 KB, and de-duplicated by provider message id so a replay changes nothing. An endpoint returns 404 unless its provider is configured. Login links are user-requested and are still delivered after a STOP.

### Meta

1. In the Meta developer app, open **WhatsApp > Configuration > Webhook**.
2. Set the callback URL to `https://<your host>/hook/meta` and the verify token to the value of `WW_WA_VERIFY_TOKEN`. Meta sends a `GET` with `hub.challenge`; webwatcher echoes it when the token matches.
3. Set `WW_WA_APP_SECRET` to the app secret (App settings > Basic). Webwatcher verifies `X-Hub-Signature-256` (HMAC-SHA256 of the raw body) on every `POST`.
4. Subscribe to the `messages` field. Restart webwatcher after changing the environment file.

### Twilio

1. Set `WW_TWILIO_TOKEN` (the same auth token used for sending). Make sure `WW_BASE_URL` is exactly the public URL Twilio calls, because the signature covers the full URL.
2. In the Twilio console, set the sender's (or sandbox's) **When a message comes in** webhook to `https://<your host>/hook/twilio`, method `POST`.
3. Webwatcher verifies `X-Twilio-Signature` (HMAC-SHA1 over the URL plus the sorted POST parameters). It answers with an empty TwiML response; it does not send a confirmation message.

## Security notes

- Magic-link tokens and session tokens are random, stored only as SHA-256 hashes, compared in constant time, and never logged (`/auth/` paths are masked in logs). Opening a link shows a button; only pressing it logs you in, so link-preview bots cannot use up the token.
- Login requests are rate limited per number and per IP, and the response is identical whether or not a number is known.
- All forms carry CSRF tokens. All SQL is parameterised. Pages are `html/template` auto-escaped, with `Content-Security-Policy: default-src 'self'`, `X-Frame-Options: DENY`, no inline scripts, 64 KB form limit and request timeouts.
- User-supplied URLs are fetched only by the scheduler. URLs with credentials, ports other than 80/443, or IP-literal hosts are rejected. The fetcher refuses loopback, private, link-local, CGNAT, multicast and cloud-metadata addresses, re-checks on every redirect, and checks again at connect time, so DNS rebinding cannot get through. Proxy environment variables are ignored.
- `/healthz` needs `WW_ADMIN_TOKEN`.

## Privacy

Webwatcher stores your WhatsApp number, when you agreed to receive messages, the URLs (and labels, selectors, targets) you watch, and the price history it collects. Nothing else: no analytics, only the login and form-security cookies. Messages go via the WhatsApp provider you configure.

- **Logs** never contain full URLs, query strings or phone numbers: they carry the host and the watch id, and numbers are masked to the last three digits. The add-URL form warns when a URL's query string looks like a token, session or booking reference (keys such as token, key, session, booking, ref, auth) and asks for a confirmation tick, because the whole URL is stored and sent to the site.
- **Retention:** price history older than 90 days (`WW_RETAIN_HISTORY_DAYS`) and check logs and finished outbox rows older than 30 days (`WW_RETAIN_DIAG_DAYS`) are purged daily.
- **Backups:** one snapshot a day, AES-256-GCM encrypted when `WW_BACKUP_KEY` is set. The directory is mode 0700 and the files 0600, as is the database. **Backups expire after 7 days** and are deleted automatically. Data you delete from the live database therefore remains in backups for up to 7 days. Keep the backup key separate from the backups.
- **Delete account** (Account page) erases your number, sessions, watches, price history, check logs, queued and sent alert records and login tokens immediately. **Log out everywhere** revokes all your sessions.

## Known limitations

- Delivery is at least once. If the process dies after a provider accepts a message but before the outbox row is updated, that alert is sent again after restart. "Accepted" means the provider took the message, not that WhatsApp delivered it; there are no delivery receipts.
- An alert that fails 5 times is marked failed and not retried; the price baseline has already moved on, so it is not re-raised.
- Login limiter and per-host rate state are in memory (bounded, but reset on restart). The global send budget is persisted.
- Currency and basis detection is heuristic. Anything undetected is `unknown`, and a change from `unknown` to a detected basis is flagged for confirmation like any other basis change, so expect an occasional confirm click after upgrades or site changes.
- When a page lists several offers, the cheapest in-stock offer in the first offer's currency is used; sold-out detection relies on JSON-LD availability only.
- Webhook STOP handling sends no confirmation reply. STOP keywords are matched on the whole message, in English only.
- Plain HTTP fetching only (no JavaScript), a single SQLite connection and a single instance. Backups are on the same host unless you copy them elsewhere.
- Existing accounts created before the consent tick have no recorded consent and receive no alerts until they log in again and tick the box.
- robots.txt is only checked with `WW_RESPECT_ROBOTS=1`; the operator is responsible for each site's terms.

## Development

```sh
WW_DEV=1 WW_DIR=/tmp/ww go run .     # (log messenger, http base URL: dev only) then open http://127.0.0.1:8780/login and /dev for the login link
go vet ./... && go test -race ./...
```

Tests make no network calls to real sites or WhatsApp; they use `httptest` servers and fakes.

## Releases

CI only. Bump `VERSION` on `main`; the `release` workflow tests, builds static `linux/amd64` and `linux/arm64` binaries with checksums, and creates the GitHub release (the tag is created server-side). Tags are never pushed by hand.
