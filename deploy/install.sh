#!/bin/sh
# Webwatcher installer for a Debian VPS running Caddy. Run as root. Idempotent.
#
#   sudo sh deploy/install.sh --domain watch.example.com [--binary /path/to/webwatcher] [--version vX.Y.Z]
#   sh deploy/install.sh --domain watch.example.com --dry-run      # print actions only
#
# Without --binary, the latest GitHub release asset for this CPU is downloaded
# and its SHA-256 checked against the release's SHA256SUMS (it fails if either
# is missing). For a private repository export GITHUB_TOKEN first.
# It never asks for, reads or prints provider secrets: edit /etc/webwatcher.env yourself.
set -eu

REPO="2E0LXY/webwatcher"
BIN_DST="/usr/local/bin/webwatcher"
ENV_FILE="/etc/webwatcher.env"
STATE_DIR="/var/lib/webwatcher"
UNIT_DST="/etc/systemd/system/webwatcher.service"
CADDYFILE="/etc/caddy/Caddyfile"
CADDY_DIR="/etc/caddy/conf.d"
CADDY_SNIPPET="$CADDY_DIR/webwatcher.caddy"
RAW="https://raw.githubusercontent.com/$REPO/main/deploy"

DOMAIN=""
BINARY=""
TAG=""
DRY=0

usage() {
	sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit "${1:-2}"
}
die() { echo "error: $*" >&2; exit 1; }
say() { echo "==> $*"; }

while [ $# -gt 0 ]; do
	case "$1" in
	--domain) [ $# -ge 2 ] || usage; DOMAIN="$2"; shift 2 ;;
	--domain=*) DOMAIN="${1#*=}"; shift ;;
	--binary) [ $# -ge 2 ] || usage; BINARY="$2"; shift 2 ;;
	--binary=*) BINARY="${1#*=}"; shift ;;
	--version) [ $# -ge 2 ] || usage; TAG="$2"; shift 2 ;;
	--version=*) TAG="${1#*=}"; shift ;;
	--dry-run) DRY=1; shift ;;
	-h | --help) usage 0 ;;
	-*) echo "unknown option: $1" >&2; usage ;;
	*) [ -z "$BINARY" ] || usage; BINARY="$1"; shift ;;
	esac
done

# Host names only: letters, digits, dots and hyphens, at least one dot.
if [ -n "$DOMAIN" ]; then
	case "$DOMAIN" in
	*[!A-Za-z0-9.-]* | .* | *. | -* | *..*) die "--domain must be a plain host name such as watch.example.com" ;;
	*.*) ;;
	*) die "--domain must be a plain host name such as watch.example.com" ;;
	esac
fi
if [ -n "$TAG" ]; then
	case "$TAG" in
	*[!A-Za-z0-9._-]*) die "--version must look like v1.2.3" ;;
	esac
fi
if [ -n "$BINARY" ] && [ "$DRY" -eq 0 ] && [ ! -f "$BINARY" ]; then
	die "binary not found: $BINARY"
fi

# run prints the command in dry-run mode and executes it otherwise.
run() {
	if [ "$DRY" -eq 1 ]; then echo "[dry-run] $*"; else "$@"; fi
}

if [ "$DRY" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
	die "run as root (sudo sh $0 ...)"
fi
[ "$DRY" -eq 1 ] && say "dry run: nothing will be changed"

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" 2>/dev/null && pwd) || SCRIPT_DIR=""
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

# deploy_file NAME: print the path of a deploy file, from the checkout if
# present, otherwise downloaded from the repository.
deploy_file() {
	if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/$1" ]; then
		echo "$SCRIPT_DIR/$1"
		return
	fi
	[ "$DRY" -eq 1 ] && { echo "$RAW/$1"; return; }
	curl -fsSL "$RAW/$1" -o "$TMP/$1" || die "cannot fetch deploy/$1"
	echo "$TMP/$1"
}

curl_gh() {
	if [ -n "${GITHUB_TOKEN:-}" ]; then
		curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" "$@"
	else
		curl -fsSL "$@"
	fi
}

# ---- 1. user and state directory ----
say "system user and state directory"
if [ "$DRY" -eq 1 ] || ! id webwatcher >/dev/null 2>&1; then
	run useradd --system --home "$STATE_DIR" --shell /usr/sbin/nologin webwatcher
fi
run install -d -o webwatcher -g webwatcher -m 0700 "$STATE_DIR"

# ---- 2. binary ----
say "binary"
if [ -z "$BINARY" ]; then
	case "$(uname -m)" in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	*) die "unsupported CPU $(uname -m): build from source and pass --binary" ;;
	esac
	ASSET="webwatcher-linux-$ARCH"
	if [ "$DRY" -eq 1 ]; then
		echo "[dry-run] download $ASSET and SHA256SUMS from the latest release of $REPO${TAG:+ (tag $TAG)}, verify the checksum, fail if either is missing"
		BINARY="$TMP/$ASSET"
	else
		command -v curl >/dev/null 2>&1 || die "curl is required: apt-get install -y curl"
		command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"
		if [ -z "$TAG" ]; then
			# /releases/latest hides pre-releases, which is what CI publishes; the list is newest first.
			TAG=$(curl_gh "https://api.github.com/repos/$REPO/releases?per_page=1" |
				sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1) ||
				die "cannot list releases (private repository? export GITHUB_TOKEN, or pass --binary)"
			[ -n "$TAG" ] || die "no release found for $REPO (private repository? export GITHUB_TOKEN); or build from source and pass --binary"
		fi
		say "release $TAG ($ASSET)"
		BASE="https://github.com/$REPO/releases/download/$TAG"
		curl_gh "$BASE/$ASSET" -o "$TMP/$ASSET" || die "release $TAG has no asset $ASSET"
		curl_gh "$BASE/SHA256SUMS" -o "$TMP/SHA256SUMS" || die "release $TAG has no SHA256SUMS"
		WANT=$(awk -v a="$ASSET" '$2 == a || $2 == "*" a { print $1 }' "$TMP/SHA256SUMS")
		[ -n "$WANT" ] || die "SHA256SUMS has no entry for $ASSET"
		GOT=$(sha256sum "$TMP/$ASSET" | awk '{ print $1 }')
		[ "$WANT" = "$GOT" ] || die "checksum mismatch for $ASSET; not installing"
		say "checksum OK"
		BINARY="$TMP/$ASSET"
	fi
fi
run install -m 0755 -o root -g root "$BINARY" "$BIN_DST.new"
run mv -f "$BIN_DST.new" "$BIN_DST"

# ---- 3. systemd unit ----
say "systemd unit"
UNIT_SRC=$(deploy_file webwatcher.service)
run install -m 0644 -o root -g root "$UNIT_SRC" "$UNIT_DST"
run systemctl daemon-reload

# ---- 4. environment file (only if absent) ----
say "environment file"
if [ -e "$ENV_FILE" ]; then
	echo "$ENV_FILE exists: left untouched"
else
	if [ "$DRY" -eq 1 ]; then
		echo "[dry-run] create $ENV_FILE (0640 root:webwatcher) with generated WW_ADMIN_TOKEN, WW_DIR, WW_BASE_URL=https://${DOMAIN:-watch.example.com}"
	else
		command -v openssl >/dev/null 2>&1 || die "openssl is required: apt-get install -y openssl"
		TOKEN=$(openssl rand -hex 32)
		install -m 0640 -o root -g webwatcher /dev/null "$ENV_FILE"
		cat >"$ENV_FILE" <<ENVEOF
# Webwatcher settings. Edit this file yourself; the installer never touches it again.
# After editing: sudo systemctl restart webwatcher && sudo webwatcher doctor
WW_ADMIN_TOKEN=$TOKEN
WW_DIR=$STATE_DIR
WW_BASE_URL=https://${DOMAIN:-watch.example.com}

# Choose ONE provider and uncomment its lines.
# Twilio WhatsApp sandbox (quickest, no template approval):
#WW_MESSENGER=twilio
#WW_TWILIO_SID=
#WW_TWILIO_TOKEN=
#WW_TWILIO_FROM=+14155238886
# Meta WhatsApp Cloud API (production, approved templates):
#WW_MESSENGER=meta
#WW_WA_PHONE_ID=
#WW_WA_TOKEN=
#WW_WA_APP_SECRET=
#WW_WA_VERIFY_TOKEN=

# Recommended: encrypts backups. Generate with: openssl rand -base64 32
#WW_BACKUP_KEY=

# Development only. The log messenger writes login links to the log and the
# service refuses to start with it unless WW_DEV=1 and the base URL is http.
#WW_MESSENGER=log
#WW_DEV=1
ENVEOF
		chmod 0640 "$ENV_FILE"
		chown root:webwatcher "$ENV_FILE"
	fi
fi

# ---- 5. Caddy ----
if [ -n "$DOMAIN" ]; then
	say "Caddy snippet for $DOMAIN"
	CADDY_OK=1
	if [ "$DRY" -eq 0 ] && ! command -v caddy >/dev/null 2>&1; then
		echo "caddy is not installed: skipping the Caddy step (apt-get install -y caddy, then re-run)"
		CADDY_OK=0
	fi
	if [ "$CADDY_OK" -eq 1 ]; then
		CADDY_SRC=$(deploy_file Caddyfile.webwatcher)
		if [ "$DRY" -eq 1 ]; then
			echo "[dry-run] back up $CADDYFILE to $CADDYFILE.bak.<timestamp>"
			echo "[dry-run] write $CADDY_SNIPPET from Caddyfile.webwatcher with host $DOMAIN"
			echo "[dry-run] append 'import $CADDY_DIR/*.caddy' to $CADDYFILE if missing"
			echo "[dry-run] caddy validate --config $CADDYFILE --adapter caddyfile; systemctl reload caddy only if valid (otherwise roll back)"
		else
			STAMP=$(date +%Y%m%d%H%M%S)
			[ -f "$CADDYFILE" ] || die "$CADDYFILE not found"
			cp -p "$CADDYFILE" "$CADDYFILE.bak.$STAMP"
			say "backed up to $CADDYFILE.bak.$STAMP"
			install -d -m 0755 "$CADDY_DIR"
			[ ! -f "$CADDY_SNIPPET" ] || cp -p "$CADDY_SNIPPET" "$TMP/snippet.prev"
			sed "s/watch\.example\.com/$DOMAIN/g" "$CADDY_SRC" >"$CADDY_SNIPPET"
			chmod 0644 "$CADDY_SNIPPET"
			# The snippet logs to /var/log/caddy; make sure Caddy can write there.
			if id caddy >/dev/null 2>&1; then install -d -o caddy -g caddy -m 0755 /var/log/caddy; fi
			if ! grep -Eq "^[[:space:]]*import[[:space:]]+(/etc/caddy/)?conf\.d/" "$CADDYFILE"; then
				printf '\nimport %s/*.caddy\n' "$CADDY_DIR" >>"$CADDYFILE"
				say "added an import line to $CADDYFILE"
			fi
			if caddy validate --config "$CADDYFILE" --adapter caddyfile >"$TMP/validate.log" 2>&1; then
				systemctl reload caddy && say "Caddy reloaded"
			else
				cat "$TMP/validate.log" >&2
				cp -p "$CADDYFILE.bak.$STAMP" "$CADDYFILE"
				if [ -f "$TMP/snippet.prev" ]; then cp -p "$TMP/snippet.prev" "$CADDY_SNIPPET"; else rm -f "$CADDY_SNIPPET"; fi
				die "Caddy config did not validate; rolled back, Caddy not reloaded"
			fi
		fi
	fi
else
	say "no --domain given: skipping Caddy (re-run with --domain watch.example.com)"
fi

# ---- 6. enable and start ----
say "service"
run systemctl enable webwatcher
if [ "$DRY" -eq 1 ]; then
	echo "[dry-run] run 'webwatcher doctor'; start (or restart) the service only if it passes"
elif "$BIN_DST" doctor >"$TMP/doctor.log" 2>&1; then
	systemctl restart webwatcher
	say "started"
else
	cat "$TMP/doctor.log"
	echo "Not started yet: the settings above need attention (expected on a first install)."
fi

cat <<NEXT

Next steps
  1. Point DNS for ${DOMAIN:-your domain} at this server (A/AAAA record).
  2. sudo nano $ENV_FILE
       set WW_MESSENGER and the provider values (Twilio sandbox or Meta); see the README Quick start.
       optional: WW_BACKUP_KEY=\$(openssl rand -base64 32)
  3. sudo webwatcher doctor        # every line should say OK; paste the webhook URLs it prints into Twilio or Meta
  4. sudo systemctl restart webwatcher
  5. sudo -u webwatcher webwatcher adduser +447700900123    # prints your one-time login link
  6. Logs: journalctl -u webwatcher -f
NEXT
