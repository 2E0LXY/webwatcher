# webwatcher

A small self-hosted price watcher. Paste the URL of a product, holiday or flight page, and webwatcher polls it. When the price changes it sends you a WhatsApp message, and if you set a target price you get a separate "below target" alert.

One static Go binary (Go standard library plus `modernc.org/sqlite` and `golang.org/x/net/html`), SQLite storage, server-rendered HTML with plain CSS and no JavaScript. It listens on `127.0.0.1` and is meant to sit behind Caddy.

## What it does

- Login by magic link sent over WhatsApp. No passwords. Single-use links that expire after 15 minutes; sessions last 30 days.
- Dashboard: add a URL (optional label, CSS selector, target price, check interval), see last price, last checked time and status, pause, resume, edit target, delete, and view price history. Up to 30 watches per user.
- Price extraction, in order: your CSS selector, then JSON-LD `Product`/`Offer`, then `itemprop`/`og:price:amount`/`product:price:amount` meta tags, then a heuristic search of the page text for `£` amounts (preferring "from £N" style prices and ignoring "was", "save" and "deposit" amounts).
- Check intervals: 30 minutes, 1 hour (default), 3 hours, 6 hours or 24 hours, with jitter and exponential backoff on errors. After 10 failures in a row the watch stops and you are told once.
- Alerts: price changed by at least 1p ("changed"), and price at or below target when the previous price was above it, or the first check was already below ("below target"). One price move sends one message (below target takes priority over changed), and the same alert is never repeated for the same price.

## Honest limitations

- **Plain HTTP only.** Webwatcher fetches the page the way a simple HTTP client does. It does not run JavaScript. Many travel sites, including **Jet2, Jet2holidays, TUI and easyJet**, build prices in the browser and/or sit behind bot protection (Akamai, Cloudflare and similar). Expect some of those pages to fail.
- **Bot protection is respected, not evaded.** If a site answers 403/429 or serves a bot-challenge page, the watch is marked `blocked` and the dashboard tells you so. Webwatcher uses an honest `webwatcher/<version>` User-Agent and will never spoof browsers, rotate proxies or use stealth tricks. A blocked page cannot be watched.
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
WW_ADMIN_TOKEN=some-long-random-string
```

Data lives in `WW_DIR`: `webwatcher.db` (SQLite, WAL mode) and `backups/` (a daily snapshot, newest 7 kept).

### Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `WW_ADDR` | `127.0.0.1:8780` | Listen address. Keep it on loopback behind Caddy. |
| `WW_DIR` | `./data` | Data directory (database and backups). |
| `WW_BASE_URL` | `http://<WW_ADDR>` | Public URL, used in login links and alerts. Use `https://` in production; cookies get the `Secure` flag (and `__Host-` prefix) when it is HTTPS. |
| `WW_ADMIN_TOKEN` | empty | Bearer token for `/healthz`. If empty, `/healthz` returns 404. |
| `WW_MESSENGER` | `log` | `log`, `meta` or `twilio`. |
| `WW_DEV` | unset | `1` shows `/dev` with captured login links, only with the `log` messenger. Never use in production. |
| `WW_WA_PHONE_ID` | | Meta: WhatsApp Business phone number ID. |
| `WW_WA_TOKEN` | | Meta: permanent access token. |
| `WW_WA_TPL_LOGIN` | `webwatcher_login` | Meta: name of the approved login template. |
| `WW_WA_TPL_ALERT` | `webwatcher_alert` | Meta: name of the approved alert template. |
| `WW_WA_LANG` | `en_GB` | Meta: template language code. |
| `WW_TWILIO_SID` | | Twilio account SID. |
| `WW_TWILIO_TOKEN` | | Twilio auth token. |
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

## Security notes

- Magic-link tokens and session tokens are random, stored only as SHA-256 hashes, compared in constant time, and never logged (`/auth/` paths are masked in logs). Opening a link shows a button; only pressing it logs you in, so link-preview bots cannot use up the token.
- Login requests are rate limited per number and per IP, and the response is identical whether or not a number is known.
- All forms carry CSRF tokens. All SQL is parameterised. Pages are `html/template` auto-escaped, with `Content-Security-Policy: default-src 'self'`, `X-Frame-Options: DENY`, no inline scripts, 64 KB form limit and request timeouts.
- User-supplied URLs are fetched only by the scheduler. URLs with credentials, ports other than 80/443, or IP-literal hosts are rejected. The fetcher refuses loopback, private, link-local, CGNAT, multicast and cloud-metadata addresses, re-checks on every redirect, and checks again at connect time, so DNS rebinding cannot get through. Proxy environment variables are ignored.
- `/healthz` needs `WW_ADMIN_TOKEN`.

## Privacy

Webwatcher stores your WhatsApp number, the URLs (and labels, selectors, targets) you watch, and the price history it collects. Nothing else: no analytics, only the login and form-security cookies. Messages go via the WhatsApp provider you configure. The **Account** page has a **Delete account** button that erases your number, sessions, watches, price history and alert records immediately.

## Development

```sh
WW_DEV=1 WW_DIR=/tmp/ww go run .     # then open http://127.0.0.1:8780/login and /dev for the login link
go vet ./... && go test -race ./...
```

Tests make no network calls to real sites or WhatsApp; they use `httptest` servers and fakes.

## Releases

CI only. Bump `VERSION` on `main`; the `release` workflow tests, builds static `linux/amd64` and `linux/arm64` binaries with checksums, and creates the GitHub release (the tag is created server-side). Tags are never pushed by hand.
