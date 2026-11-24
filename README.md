# webwatcher

A small self-hosted price watcher. Paste the URL of a product, holiday or flight page, and webwatcher polls it. When the price changes it sends you a WhatsApp message, and if you set a target price you get a separate "below target" alert.

One static Go binary (Go standard library plus `modernc.org/sqlite` and `golang.org/x/net/html`), SQLite storage, server-rendered HTML with plain CSS and no JavaScript. It listens on `127.0.0.1` and is meant to sit behind Caddy.

## What it does

- Login by magic link sent over WhatsApp. No passwords. Single-use links that expire after 15 minutes; sessions last 30 days. The login form has a required opt-in tick ("I agree to receive WhatsApp messages from Webwatcher about my watches"), recorded with a timestamp. **Log out everywhere** on the Account page revokes every session.
- Consent and opt-out: no alert is ever sent without a recorded opt-in. Alerts can be switched off on the dashboard, and replying STOP (or UNSUBSCRIBE) to a message does the same through the provider webhook; START turns them back on. Prices keep being tracked while alerts are off.
- Dashboard: add a URL (optional label, CSS selector, target price, check interval), see last price, last checked time and status, pause, resume, edit target, delete, and view price history. Up to 30 watches per user.
- Price extraction, in order: your CSS selector, then JSON-LD `Product`/`Offer`, then `itemprop`/`og:price:amount`/`product:price:amount` meta tags, then a heuristic search of the page text for `£` amounts (preferring "from £N" style prices and ignoring "was", "save" and "deposit" amounts).
- Check intervals: 30 minutes, 1 hour (default), 3 hours, 6 hours or 24 hours, with jitter and exponential backoff on errors. After 10 failures in a row the watch stops and you are told once.
- Alerts fire on transitions: price changed by at least 1p ("changed"), and price crossing from above the target to at or below it ("below target"). A later recovery above the target re-arms the below-target alert, and so does editing the target. One price move sends one message (below target takes priority over changed), and an unchanged price never repeats an alert.
- Alerts are written to a transactional outbox in the same database transaction as the price update, then delivered by a sender loop with up to 5 attempts and exponential backoff. They survive restarts. Before a queued alert is sent the watch and owner are re-checked: if the watch was deleted, paused, had its target edited, or the owner opted out, the alert is dropped. A global budget (`WW_SEND_BUDGET_PER_HOUR`, default 200) caps outbound messages per hour; login links may use at most half of it so they cannot starve alerts.
- Price basis: every observation is stored with its currency and, when detectable from JSON-LD or the wording ("per person", "pp", "total", "for 2 adults"), its basis. Observations with a different currency or basis are never compared: the watch is flagged **Basis changed** on the dashboard, no alert is sent, and you confirm with a button that resets the baseline.
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

Webwatcher refuses to start unless it is configured for production: `WW_BASE_URL` must be `https://` and `WW_MESSENGER` must be `meta` or `twilio`. Only with `WW_DEV=1` are an `http://` base URL and the `log` messenger allowed.

### Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `WW_ADDR` | `127.0.0.1:8780` | Listen address. Keep it on loopback behind Caddy. |
| `WW_DIR` | `./data` | Data directory (database and backups). |
| `WW_BASE_URL` | `http://<WW_ADDR>` | Public URL, used in login links, alerts and Twilio signature checks. Must be `https://` unless `WW_DEV=1`; cookies get the `Secure` flag (and `__Host-` prefix) when it is HTTPS. |
| `WW_ADMIN_TOKEN` | empty | Bearer token for `/healthz`. If empty, `/healthz` returns 404. |
| `WW_MESSENGER` | `log` | `log`, `meta` or `twilio`. `log` is refused unless `WW_DEV=1` and the base URL is http. |
| `WW_DEV` | unset | `1` allows an http base URL and the `log` messenger, and shows `/dev` with captured login links. Never use in production. |
| `WW_SEND_BUDGET_PER_HOUR` | `200` | Global cap on outbound WhatsApp messages per rolling hour (alerts and login links). Over-budget alerts stay queued. |
| `WW_RETAIN_HISTORY_DAYS` | `90` | Price history older than this is purged daily. |
| `WW_RETAIN_DIAG_DAYS` | `30` | Check logs and finished outbox rows older than this are purged daily. |
| `WW_BACKUP_KEY` | empty | 32 random bytes, base64 (`openssl rand -base64 32`). If set, backups are AES-256-GCM encrypted. If empty, backups are plaintext (still mode 0600) and a warning is logged. |
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
