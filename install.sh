#!/usr/bin/env bash
#
# kiro-proxy installer — one-command setup.
#
#   curl -fsSL https://raw.githubusercontent.com/RobinGasi/kiro-proxy/main/install.sh | bash
#
# What it does:
#   1. Detects your Kiro credentials and how you log in — AWS IAM Identity Center
#      / Builder ID / social login (via the kiro-cli SQLite DB), a Kiro IDE
#      credentials file, or a raw refresh token. This is the "SSO vs not"
#      detection; the proxy then auto-selects the matching refresh flow.
#   2. Writes ~/.kiro-proxy/.env pointing at that source.
#   3. Installs and starts the proxy: Docker (a managed, auto-restarting
#      container) or a native binary built from source — auto-chosen, with
#      fallback.
#   4. Health-checks it and prints how to point Claude Code / Codex at it.
#
# If no credentials are found and `kiro-cli` is installed, it offers to run
# `kiro-cli login` and (best-effort) to ask the kiro-cli AI for help. When `omp`
# (oh-my-pi) is installed, it also links the bundled omp-kiro plugin so Kiro
# models show up in omp.
#
# Everything is overridable via flags or env vars — run with --help.
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults (override with env vars or flags)
# ---------------------------------------------------------------------------
REGISTRY="${KIRO_PROXY_REGISTRY:-ghcr.io}"
REPO="${KIRO_PROXY_REPO:-RobinGasi/kiro-proxy}"
REF="${KIRO_PROXY_REF:-main}"
DIR="${KIRO_PROXY_DIR:-$HOME/.kiro-proxy}"
PORT="${KIRO_PROXY_PORT:-8442}"
REGION="${KIRO_REGION:-us-east-1}"
METHOD="${KIRO_PROXY_METHOD:-auto}"     # auto | docker | source
ASSUME_YES=0
ASSIST=0
NO_START=0
OMP_PLUGIN="${KIRO_PROXY_OMP:-1}"        # 1 = install the omp-kiro plugin when omp exists
REFRESH_TOKEN="${REFRESH_TOKEN:-}"
PROFILE_ARN="${PROFILE_ARN:-}"

# Detection results (filled in by detect_credentials).
CRED_KIND=""     # sqlite | creds_file | refresh_token | none
CRED_PATH=""
CRED_LOGIN=""    # sso | builder-id | social | unknown
PROXY_API_KEY=""

ENV_FILE="$DIR/.env"

# ---------------------------------------------------------------------------
# Pretty output
# ---------------------------------------------------------------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'; C_DIM=$'\033[2m'
  C_BLUE=$'\033[34m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'
else
  C_RESET=""; C_BOLD=""; C_DIM=""; C_BLUE=""; C_GREEN=""; C_YELLOW=""; C_RED=""
fi
log()  { printf '%s==>%s %s\n' "$C_BLUE$C_BOLD" "$C_RESET" "$*"; }
ok()   { printf '%s  ✓%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%s  !%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
err()  { printf '%s  ✗%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }
die()  { err "$*"; exit 1; }

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }
have()  { command -v "$1" >/dev/null 2>&1; }

# Interactive input works even under `curl | bash` by reading from /dev/tty.
TTY=""
[ -r /dev/tty ] && TTY=/dev/tty
ask() { # ask "prompt" "default" -> echoes the answer
  local prompt="$1" def="${2:-}" ans=""
  if [ "$ASSUME_YES" = 1 ] || [ -z "$TTY" ]; then printf '%s' "$def"; return; fi
  printf '%s%s%s' "$C_BOLD" "$prompt" "$C_RESET" > /dev/tty
  IFS= read -r ans < "$TTY" || ans=""
  [ -z "$ans" ] && ans="$def"
  printf '%s' "$ans"
}
confirm() { # confirm "prompt" "y|n default" -> exit 0 for yes
  case "$(lower "$(ask "$1" "$2")")" in y|yes) return 0 ;; *) return 1 ;; esac
}

usage() {
  cat <<EOF
${C_BOLD}kiro-proxy installer${C_RESET}

Usage: install.sh [options]

Options:
  --dir PATH            Install/config dir            (default: $HOME/.kiro-proxy)
  --port N              Port to expose                (default: 8442)
  --region REGION       KIRO_REGION                   (default: us-east-1)
  --method M            auto | docker | source        (default: auto)
  --refresh-token TOK   Use a raw Kiro refresh token as the credential source
  --profile-arn ARN     CodeWhisperer profile ARN (used with --refresh-token)
  --repo OWNER/NAME     Source repository             (default: $REPO)
  --ref REF             Branch/tag to build           (default: main)
  --assist              If setup gets stuck, ask the kiro-cli AI for help
  -y, --yes             Non-interactive; accept all defaults
  --no-start            Configure and install, but don't start the proxy
  --no-omp              Skip installing the omp-kiro plugin for omp
  -h, --help            Show this help

Env var overrides: KIRO_PROXY_DIR, KIRO_PROXY_PORT, KIRO_REGION,
KIRO_PROXY_METHOD, KIRO_PROXY_REPO, KIRO_PROXY_REF, KIRO_PROXY_REGISTRY,
KIRO_PROXY_OMP (0 = skip omp plugin), REFRESH_TOKEN, PROFILE_ARN.
EOF
}

# ---------------------------------------------------------------------------
# Flag parsing
# ---------------------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) DIR="$2"; ENV_FILE="$DIR/.env"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --region) REGION="$2"; shift 2 ;;
    --method) METHOD="$2"; shift 2 ;;
    --refresh-token) REFRESH_TOKEN="$2"; shift 2 ;;
    --profile-arn) PROFILE_ARN="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    --ref) REF="$2"; shift 2 ;;
    --assist) ASSIST=1; shift ;;
    -y|--yes) ASSUME_YES=1; shift ;;
    --no-start) NO_START=1; shift ;;
    --no-omp) OMP_PLUGIN=0; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

OWNER="$(lower "${REPO%%/*}")"
IMAGE="$REGISTRY/$OWNER/kiro-proxy:latest"

# ---------------------------------------------------------------------------
# Platform
# ---------------------------------------------------------------------------
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac

# ---------------------------------------------------------------------------
# Credential detection — the "SSO vs not" step
# ---------------------------------------------------------------------------
db_candidates() {
  local xdg="${XDG_DATA_HOME:-$HOME/.local/share}"
  printf '%s\n' \
    "$xdg/kiro-cli/data.sqlite3" \
    "$HOME/.local/share/kiro-cli/data.sqlite3" \
    "$HOME/Library/Application Support/kiro-cli/data.sqlite3" \
    "$xdg/amazon-q/data.sqlite3" \
    "$HOME/.local/share/amazon-q/data.sqlite3" \
    "$HOME/Library/Application Support/amazon-q/data.sqlite3"
}

creds_candidates() {
  printf '%s\n' \
    "$HOME/.aws/sso/cache/kiro-auth-token.json" \
    "$HOME/Library/Application Support/kiro/credentials.json" \
    "$HOME/.kiro/credentials.json"
}

# classify_db DB -> sso | builder-id | social | unknown (cosmetic; never fails).
classify_db() {
  local db="$1" keys=""
  have sqlite3 || { printf 'unknown'; return; }
  keys="$(sqlite3 "file:$db?mode=ro" 'SELECT key FROM auth_kv;' 2>/dev/null || true)"
  case "$keys" in
    *kirocli:odic*|*codewhisperer:odic*) printf 'sso' ;;
    *kirocli:social*)                    printf 'social' ;;
    "" )                                 printf 'unknown' ;;
    *)                                   printf 'unknown' ;;
  esac
}

login_label() {
  case "$1" in
    sso)        printf 'AWS IAM Identity Center / corporate SSO' ;;
    builder-id) printf 'AWS Builder ID' ;;
    social)     printf 'Builder ID / social login (Google, GitHub, Microsoft, ...)' ;;
    *)          printf 'login type unknown (the proxy will auto-detect it)' ;;
  esac
}

detect_credentials() {
  # 0) Explicit refresh token wins.
  if [ -n "$REFRESH_TOKEN" ]; then
    CRED_KIND="refresh_token"; CRED_LOGIN="unknown"
    return
  fi

  # 1) kiro-cli SQLite DB — the richest source; covers every login type.
  local db
  while IFS= read -r db; do
    [ -f "$db" ] || continue
    CRED_KIND="sqlite"; CRED_PATH="$db"; CRED_LOGIN="$(classify_db "$db")"
    return
  done <<EOF
$(db_candidates)
EOF

  # 2) A Kiro IDE credentials JSON file, if one looks valid.
  local f
  while IFS= read -r f; do
    [ -f "$f" ] || continue
    if grep -Eq '"(refreshToken|clientId|clientIdHash)"' "$f" 2>/dev/null; then
      CRED_KIND="creds_file"; CRED_PATH="$f"; CRED_LOGIN="unknown"
      return
    fi
  done <<EOF
$(creds_candidates)
EOF

  # 3) Re-run: keep the source a previous run wrote to $ENV_FILE (e.g. a pasted
  #    refresh token), so a second install needs no prompts.
  local prev
  prev="$(env_value REFRESH_TOKEN)"
  if [ -n "$prev" ]; then
    REFRESH_TOKEN="$prev"; PROFILE_ARN="${PROFILE_ARN:-$(env_value PROFILE_ARN)}"
    CRED_KIND="refresh_token"; CRED_LOGIN="unknown"
    return
  fi
  prev="$(env_value KIRO_CLI_DB_FILE)"
  if [ -n "$prev" ] && [ -f "$prev" ]; then
    CRED_KIND="sqlite"; CRED_PATH="$prev"; CRED_LOGIN="$(classify_db "$prev")"
    return
  fi
  prev="$(env_value KIRO_CREDS_FILE)"
  if [ -n "$prev" ] && [ -f "$prev" ]; then
    CRED_KIND="creds_file"; CRED_PATH="$prev"; CRED_LOGIN="unknown"
    return
  fi

  CRED_KIND="none"
}

# ---------------------------------------------------------------------------
# kiro-cli helpers (login + best-effort AI assist)
# ---------------------------------------------------------------------------
kiro_login() {
  have kiro-cli || return 1
  [ -n "$TTY" ] || { warn "No terminal available for interactive login."; return 1; }
  log "Launching 'kiro-cli login' (choose Builder ID, IdC/SSO, or social)…"
  kiro-cli login < "$TTY" > "$TTY" 2>&1 || return 1
}

# kiro_assist "question" — best-effort: probe kiro-cli's chat surface and ask.
kiro_assist() {
  local q="$1" help=""
  have kiro-cli || { warn "kiro-cli not installed; can't ask its AI. See https://kiro.dev"; return 1; }
  log "Asking the kiro-cli AI for help…"
  help="$(kiro-cli chat --help 2>/dev/null || kiro-cli help chat 2>/dev/null || true)"
  if printf '%s' "$help" | grep -q -- '--no-interactive'; then
    kiro-cli chat --no-interactive "$q" < /dev/null || true
  elif printf '%s' "$help" | grep -Eqi 'usage|prompt|message'; then
    kiro-cli chat "$q" < /dev/null 2>/dev/null || kiro-cli chat "$q" || true
  elif [ -n "$TTY" ]; then
    warn "No scriptable chat mode found; opening an interactive kiro-cli chat."
    printf '%s\n' "$q" > "$TTY"
    kiro-cli chat < "$TTY" > "$TTY" 2>&1 || true
  else
    warn "Could not reach the kiro-cli AI non-interactively."
    return 1
  fi
}

# ---------------------------------------------------------------------------
# Resolve a credential source, prompting / assisting when nothing is found
# ---------------------------------------------------------------------------
resolve_credentials() {
  detect_credentials
  if [ "$CRED_KIND" != "none" ]; then
    case "$CRED_KIND" in
      sqlite)       ok "Found kiro-cli database: $CRED_PATH" ; ok "Login type: $(login_label "$CRED_LOGIN")" ;;
      creds_file)   ok "Found Kiro credentials file: $CRED_PATH" ;;
      refresh_token) ok "Using the refresh token you provided" ;;
    esac
    return
  fi

  warn "No Kiro credentials were found on this machine."
  if [ "$ASSUME_YES" = 1 ] || [ -z "$TTY" ]; then
    die "Running non-interactively with no credentials. Pass --refresh-token TOKEN (and --profile-arn), or set a source in $ENV_FILE, then re-run. See the README for options."
  fi

  # Offer kiro-cli login first — it handles Builder ID / IdC-SSO / social natively.
  if have kiro-cli; then
    if confirm "Log in now with 'kiro-cli login'? [Y/n] " "y"; then
      if kiro_login; then detect_credentials; fi
      if [ "$CRED_KIND" != "none" ]; then ok "Credentials detected after login."; return; fi
      warn "Still no credentials after login."
    fi
  else
    warn "kiro-cli is not installed. Install Kiro / kiro-cli from https://kiro.dev to log in,"
    warn "or paste a refresh token below."
  fi

  # Manual: paste a refresh token.
  if confirm "Paste a Kiro refresh token instead? [y/N] " "n"; then
    REFRESH_TOKEN="$(ask "  Refresh token: " "")"
    PROFILE_ARN="$(ask "  Profile ARN (recommended, blank to skip): " "")"
    if [ -n "$REFRESH_TOKEN" ]; then CRED_KIND="refresh_token"; CRED_LOGIN="unknown"; ok "Refresh token captured."; return; fi
  fi

  # Last resort: the kiro-cli AI.
  if [ "$ASSIST" = 1 ] || confirm "Ask the kiro-cli AI how to get credentials? [y/N] " "n"; then
    kiro_assist "I'm setting up kiro-proxy on $OS and it found no Kiro credentials. How do I log in with kiro-cli, and where does it store the token database this tool can read?"
  fi
  die "No credential source configured. Set one in $ENV_FILE and re-run, or see the README."
}

# ---------------------------------------------------------------------------
# .env generation
# ---------------------------------------------------------------------------
gen_key() {
  if have openssl; then openssl rand -hex 24; return; fi
  if [ -r /dev/urandom ]; then LC_ALL=C tr -dc 'a-f0-9' < /dev/urandom | head -c 48; echo; return; fi
  printf 'kp-%s-%s\n' "$$" "$(date +%s)"
}

# env_value KEY -> value of KEY in $ENV_FILE ("" when absent).
env_value() {
  [ -f "$ENV_FILE" ] || return 0
  sed -n "s/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}$1=//p" "$ENV_FILE" | head -n1
}

# Keys this script owns; every other assignment in $ENV_FILE is user config and
# survives re-runs verbatim.
MANAGED_KEYS=" SERVER_HOST SERVER_PORT PROXY_API_KEY KIRO_REGION KIRO_API_HOST KIRO_Q_HOST KIRO_MAX_PAYLOAD_BYTES AUTO_TRIM_PAYLOAD KIRO_CLI_DB_FILE SQLITE_READONLY KIRO_CREDS_FILE REFRESH_TOKEN PROFILE_ARN "

# user_env_lines -> the KEY=VALUE lines of $ENV_FILE not owned by this script.
user_env_lines() {
  [ -f "$ENV_FILE" ] || return 0
  local line key
  while IFS= read -r line || [ -n "$line" ]; do
    key="${line#"${line%%[![:space:]]*}"}"
    key="${key#export }"
    case "$key" in [A-Za-z_]*=*) ;; *) continue ;; esac
    key="${key%%=*}"
    case "$MANAGED_KEYS" in *" $key "*) continue ;; esac
    printf '%s\n' "$line"
  done < "$ENV_FILE"
}

write_env() {
  mkdir -p "$DIR"
  PROXY_API_KEY="$(env_value PROXY_API_KEY)"
  [ -n "$PROXY_API_KEY" ] || PROXY_API_KEY="$(gen_key)"

  local credblock
  case "$CRED_KIND" in
    sqlite)
      credblock="KIRO_CLI_DB_FILE=$CRED_PATH
# Read-only avoids the refresh-token rotation race with a running kiro-cli.
SQLITE_READONLY=true" ;;
    creds_file)
      credblock="KIRO_CREDS_FILE=$CRED_PATH" ;;
    refresh_token)
      credblock="REFRESH_TOKEN=$REFRESH_TOKEN
PROFILE_ARN=$PROFILE_ARN" ;;
    *)
      credblock="# No source detected — set ONE of REFRESH_TOKEN(+PROFILE_ARN),
# KIRO_CREDS_FILE, or KIRO_CLI_DB_FILE." ;;
  esac

  local user tmp
  user="$(user_env_lines)"
  umask 077
  tmp="$(mktemp "$DIR/.env.XXXXXX")"
  cat > "$tmp" <<EOF
# Generated by kiro-proxy install.sh. Re-running the installer rewrites the
# settings it owns (PROXY_API_KEY is kept); any other KEY=VALUE you add is kept
# under "Your settings" (comments are not).
# Full annotated reference: .env.example in the repository.
SERVER_HOST=0.0.0.0
SERVER_PORT=$PORT

# Clients authenticate to the proxy with this key.
PROXY_API_KEY=$PROXY_API_KEY

KIRO_REGION=$REGION

# --- endpoint & payload tuning ---
# The proxy talks to the NATIVE AWS Q host (https://q.<region>.amazonaws.com) that
# genuine Kiro IDE / kiro-cli use. It carries far larger request bodies than the old
# legacy runtime.<region>.kiro.dev gateway (which capped at ~615KB and silently broke
# big [1m] contexts). These lines are stamped on every install run so upgrading an
# existing machine automatically migrates it off the legacy limits.
# Leave KIRO_API_HOST/KIRO_Q_HOST blank to use the native default; to roll back set
# both to https://runtime.%s.kiro.dev and lower KIRO_MAX_PAYLOAD_BYTES to 600000.
KIRO_API_HOST=
KIRO_Q_HOST=
# 8MB fits a full 1M-token conversation with headroom; it's a runaway-payload safety
# net, not a routine trimmer. AUTO_TRIM_PAYLOAD=false returns a clear "context too
# large — use /compact" error instead of silently dropping history.
KIRO_MAX_PAYLOAD_BYTES=8000000
AUTO_TRIM_PAYLOAD=false

# --- credential source (auto-detected: ${CRED_KIND}$( [ -n "$CRED_LOGIN" ] && printf ' / %s' "$CRED_LOGIN" )) ---
$credblock

# --- Your settings (kept across installer runs) ---
EOF
  [ -z "$user" ] || printf '%s\n' "$user" >> "$tmp"

  if [ -f "$ENV_FILE" ] && cmp -s "$tmp" "$ENV_FILE"; then
    rm -f "$tmp"
    ok "$ENV_FILE is up to date"
  else
    chmod 600 "$tmp"
    mv -f "$tmp" "$ENV_FILE"
    ok "Wrote $ENV_FILE"
  fi
}

# ---------------------------------------------------------------------------
# Install method: docker
# ---------------------------------------------------------------------------
docker_available() { have docker && docker info >/dev/null 2>&1; }

# fingerprint <strings…> -> stable hash of its arguments (and $ENV_FILE).
fingerprint() {
  { printf '%s\n' "$@"; cat "$ENV_FILE" 2>/dev/null; } |
    { if have sha256sum; then sha256sum; elif have shasum; then shasum -a 256; else cksum; fi; } |
    cut -d' ' -f1
}

install_docker() {
  docker_available || return 1
  log "Installing via Docker ($IMAGE)…"
  if ! docker pull "$IMAGE" >/dev/null 2>&1; then
    warn "Could not pull $IMAGE."
    return 1
  fi

  # Build the argument list as an array so paths with spaces (e.g. macOS
  # "Library/Application Support/…") survive intact.
  # Map host:container to the same port and let the app bind $PORT inside (via
  # SERVER_PORT in the env-file), so any --port works — not just the default.
  # The named volume keeps account state (model cooldowns, failure counts)
  # across container recreation; the image ships /state owned by nonroot.
  local args=(-d --name kiro-proxy --restart unless-stopped
    -p "$PORT:$PORT" --env-file "$ENV_FILE"
    -v kiro-proxy-state:/state -e ACCOUNTS_STATE_FILE=/state/state.json)

  # Mount a local credential file (SQLite/JSON) read-only and point the proxy at it.
  if [ "$CRED_KIND" = "sqlite" ] || [ "$CRED_KIND" = "creds_file" ]; then
    local cdir cbase
    cdir="$(cd "$(dirname "$CRED_PATH")" && pwd)"
    cbase="$(basename "$CRED_PATH")"
    args+=(--mount "type=bind,source=$cdir,target=/creds,readonly")
    if [ "$CRED_KIND" = "sqlite" ]; then args+=(-e "KIRO_CLI_DB_FILE=/creds/$cbase")
    else args+=(-e "KIRO_CREDS_FILE=/creds/$cbase"); fi
  fi

  # Same image, flags and .env as the running container → leave it alone.
  local want have_fp running
  want="$(fingerprint "$(docker image inspect -f '{{.Id}}' "$IMAGE" 2>/dev/null)" "${args[@]}")"
  have_fp="$(docker inspect -f '{{index .Config.Labels "dev.kiro-proxy.config"}}' kiro-proxy 2>/dev/null || true)"
  running="$(docker inspect -f '{{.State.Running}}' kiro-proxy 2>/dev/null || true)"
  if [ "$have_fp" = "$want" ] && [ "$running" = true ]; then
    RUN_METHOD="docker"
    ok "Container 'kiro-proxy' is up to date."
    return 0
  fi

  # Switching from a source install: free the port first.
  [ -x "$DIR/stop.sh" ] && "$DIR/stop.sh" >/dev/null 2>&1 || true
  docker rm -f kiro-proxy >/dev/null 2>&1 || true
  args+=(--label "dev.kiro-proxy.config=$want" "$IMAGE")

  if ! docker run "${args[@]}" >/dev/null; then
    warn "docker run failed."
    return 1
  fi
  RUN_METHOD="docker"
  ok "Container 'kiro-proxy' started."
}

# ---------------------------------------------------------------------------
# Install method: build from source
# ---------------------------------------------------------------------------
clone_source() {
  local dest="$DIR/src"
  if [ -d "$dest/.git" ]; then
    git -C "$dest" fetch --depth 1 origin "$REF" >/dev/null 2>&1 || true
    git -C "$dest" checkout -q FETCH_HEAD 2>/dev/null || git -C "$dest" checkout -q "$REF" 2>/dev/null || true
    return 0
  fi
  rm -rf "$dest"
  git clone --depth 1 --branch "$REF" "https://github.com/$REPO.git" "$dest" >/dev/null 2>&1
}

install_source() {
  have go || { warn "Go toolchain not found; can't build from source."; return 1; }
  log "Building from source ($REPO@$REF)…"
  clone_source || { warn "Clone of https://github.com/$REPO failed."; return 1; }
  local bin="$DIR/kiro-proxy" new="$DIR/.kiro-proxy.new" changed=0
  ( cd "$DIR/src" && CGO_ENABLED=0 go build -trimpath -o "$new" ./cmd/kiro-proxy ) \
    || { rm -f "$new"; warn "go build failed."; return 1; }
  if [ -f "$bin" ] && cmp -s "$new" "$bin"; then
    rm -f "$new"
    ok "$bin is up to date"
  else
    mv -f "$new" "$bin"
    changed=1
    ok "Built $bin"
  fi

  # Management scripts (native run — cross-platform, no systemd/launchd assumptions).
  cat > "$DIR/start.sh" <<EOF
#!/usr/bin/env bash
cd "$DIR" || exit 1
if [ -f "$DIR/kiro-proxy.pid" ] && kill -0 "\$(cat "$DIR/kiro-proxy.pid")" 2>/dev/null; then
  echo "already running (PID \$(cat "$DIR/kiro-proxy.pid"))"; exit 0
fi
nohup "$DIR/kiro-proxy" >"$DIR/kiro-proxy.log" 2>&1 &
echo \$! > "$DIR/kiro-proxy.pid"
echo "started (PID \$!) — logs: $DIR/kiro-proxy.log"
EOF
  cat > "$DIR/stop.sh" <<EOF
#!/usr/bin/env bash
pidfile="$DIR/kiro-proxy.pid"
pid="\$(cat "\$pidfile" 2>/dev/null)"
if [ -z "\$pid" ] || ! kill -0 "\$pid" 2>/dev/null; then
  rm -f "\$pidfile"; echo "not running"; exit 0
fi
kill "\$pid"
# Wait for the port to be released so an immediate start.sh can bind it.
for _ in \$(seq 1 50); do kill -0 "\$pid" 2>/dev/null || break; sleep 0.1; done
rm -f "\$pidfile"; echo stopped
EOF
  chmod +x "$DIR/start.sh" "$DIR/stop.sh"

  # Switching from a Docker install on the same port: free it first. A
  # kiro-proxy container on another port is left running.
  if have docker && docker port kiro-proxy 2>/dev/null | grep -q ":$PORT\$"; then
    docker rm -f kiro-proxy >/dev/null 2>&1 || true
  fi

  # A running proxy keeps serving the old binary and .env until restarted;
  # restart only when either actually changed.
  local cfg; cfg="$(fingerprint)"
  if [ "$changed" = 1 ] || [ "$(cat "$DIR/.env.applied" 2>/dev/null)" != "$cfg" ]; then
    "$DIR/stop.sh" >/dev/null
  fi
  "$DIR/start.sh"
  printf '%s\n' "$cfg" > "$DIR/.env.applied"
  RUN_METHOD="source"
}

# ---------------------------------------------------------------------------
# Health check + summary
# ---------------------------------------------------------------------------
wait_health() {
  local url="http://127.0.0.1:$PORT/health" i=0
  have curl || { sleep 2; return 0; }
  while [ "$i" -lt 40 ]; do
    curl -fsS "$url" >/dev/null 2>&1 && return 0
    sleep 0.5; i=$((i+1))
  done
  return 1
}

# ---------------------------------------------------------------------------
# omp (oh-my-pi) integration: the omp-kiro plugin registers a `kiro` provider
# backed by this proxy. It reads PROXY_API_KEY from ~/.kiro-proxy/.env and
# defaults to http://127.0.0.1:8442, so only a non-default port or dir needs
# extra config in omp's agent .env.
# ---------------------------------------------------------------------------
OMP_STATUS=""
OMP_BLOCK_BEGIN="# >>> kiro-proxy install.sh >>>"
OMP_BLOCK_END="# <<< kiro-proxy install.sh <<<"

# set_managed_block FILE [LINE…] — replace this script's marked block in FILE
# with LINEs, or drop the block when none are given. Other lines are untouched.
set_managed_block() {
  local file="$1" tmp; shift
  [ -f "$file" ] || [ $# -gt 0 ] || return 0
  mkdir -p "$(dirname "$file")"
  tmp="$(mktemp)"
  [ -f "$file" ] && awk -v b="$OMP_BLOCK_BEGIN" -v e="$OMP_BLOCK_END" \
    '$0 == b { skip = 1; next } $0 == e { skip = 0; next } !skip' "$file" > "$tmp"
  if [ $# -gt 0 ]; then
    { printf '%s\n' "$OMP_BLOCK_BEGIN"; printf '%s\n' "$@"; printf '%s\n' "$OMP_BLOCK_END"; } >> "$tmp"
  fi
  if [ -f "$file" ] && cmp -s "$tmp" "$file"; then rm -f "$tmp"; return 0; fi
  cat "$tmp" > "$file"
  rm -f "$tmp"
  chmod 600 "$file" 2>/dev/null || true
}

# fetch_omp_plugin DEST — put this repo's omp-kiro/ directory (at $REF) in DEST.
# Returns 0 when DEST is current; DEST is only replaced when its content changed.
fetch_omp_plugin() {
  local dest="$1" tmp
  tmp="$(mktemp -d)"
  if ! curl -fsSL "https://codeload.github.com/$REPO/tar.gz/$REF" | tar -xzf - -C "$tmp" 2>/dev/null \
    || [ ! -d "$tmp"/*/omp-kiro ]; then
    rm -rf "$tmp"
    return 1
  fi
  if [ -d "$dest" ] && diff -rq "$tmp"/*/omp-kiro "$dest" >/dev/null 2>&1; then
    rm -rf "$tmp"
    return 0
  fi
  rm -rf "$dest"
  mv "$tmp"/*/omp-kiro "$dest"
  rm -rf "$tmp"
}

install_omp_plugin() {
  [ "$OMP_PLUGIN" = 1 ] || return 0
  have omp || return 0
  local dest="$DIR/omp-kiro"
  log "omp found — setting up the omp-kiro plugin…"
  if ! fetch_omp_plugin "$dest"; then
    warn "Could not download the omp-kiro plugin from github.com/$REPO@$REF."
    OMP_STATUS="failed"
    return 0
  fi

  # Link (not install) so re-runs update the plugin in place. Skip when omp
  # already points at $dest: re-linking would reset its enabled state.
  local current
  current="$(omp plugin list --json 2>/dev/null | grep -o '"path": *"[^"]*/omp-kiro"' | sed 's/.*"\(.*\)"$/\1/' | head -n1)"
  if [ -z "$current" ] || [ "$(cd "$current" 2>/dev/null && pwd -P)" != "$(cd "$dest" && pwd -P)" ]; then
    # A copy installed from git/npm (e.g. github:RobinGasi/omp-kiro) blocks linking.
    if [ -n "$current" ] && [ ! -L "$current" ]; then
      omp plugin uninstall omp-kiro >/dev/null 2>&1 || true
    fi
    if ! omp plugin link "$dest" >/dev/null 2>&1; then
      warn "omp plugin link failed. Retry with: omp plugin link $dest"
      OMP_STATUS="failed"
      return 0
    fi
  fi

  local agent_env="${PI_CODING_AGENT_DIR:-$HOME/.omp/agent}/.env" lines=()
  [ "$PORT" = 8442 ] || lines+=("KIRO_PROXY_URL=http://127.0.0.1:$PORT")
  # The plugin only auto-reads ~/.kiro-proxy/.env; otherwise hand it the key.
  [ "$DIR" = "$HOME/.kiro-proxy" ] || lines+=("KIRO_PROXY_API_KEY=$PROXY_API_KEY")
  set_managed_block "$agent_env" ${lines[@]+"${lines[@]}"}

  OMP_STATUS="installed"
  ok "omp-kiro ready ($dest). Restart omp, then pick a kiro/… model (omp models)."
}

summary() {
  printf '\n%s%s kiro-proxy is set up %s\n' "$C_GREEN$C_BOLD" "──" "$C_RESET"
  printf '  %sBase URL%s  http://localhost:%s\n' "$C_DIM" "$C_RESET" "$PORT"
  printf '  %sAPI key %s  %s\n' "$C_DIM" "$C_RESET" "$PROXY_API_KEY"
  printf '  %sConfig  %s  %s\n' "$C_DIM" "$C_RESET" "$ENV_FILE"
  case "${RUN_METHOD:-}" in
    docker) printf '  %sManage  %s  docker logs -f kiro-proxy | docker restart kiro-proxy | docker rm -f kiro-proxy\n' "$C_DIM" "$C_RESET" ;;
    source) printf '  %sManage  %s  %s/start.sh | %s/stop.sh | tail -f %s/kiro-proxy.log\n' "$C_DIM" "$C_RESET" "$DIR" "$DIR" "$DIR" ;;
  esac

  cat <<EOF

${C_BOLD}List available models:${C_RESET}
  curl -s http://localhost:$PORT/v1/models -H "Authorization: Bearer $PROXY_API_KEY"

EOF

  cat <<EOF
${C_BOLD}Claude Code${C_RESET} (Anthropic-compatible):
  export ANTHROPIC_BASE_URL=http://localhost:$PORT
  export ANTHROPIC_API_KEY=$PROXY_API_KEY
  export ANTHROPIC_MODEL='opus[1m]'   # newest Opus; [1m] = 1M context window
  export ANTHROPIC_SMALL_FAST_MODEL=haiku
  claude

EOF

  cat <<EOF
${C_BOLD}Codex CLI${C_RESET} (OpenAI-compatible): add a provider with
  base_url = "http://localhost:$PORT/v1"  and  env_key = "PROXY_API_KEY"
  (see README → "Using with coding agents").
EOF

  if [ "$OMP_STATUS" = installed ]; then
    cat <<EOF

${C_BOLD}omp${C_RESET} (oh-my-pi): omp-kiro plugin ready. Restart omp, then:
  omp --model kiro/claude-opus-5 "…"
EOF
  elif [ "$OMP_PLUGIN" != 1 ]; then
    :
  elif [ "$OMP_STATUS" = failed ]; then
    cat <<EOF

${C_BOLD}omp${C_RESET} (oh-my-pi): omp-kiro was not set up — re-run this installer.
EOF
  else
    cat <<EOF

${C_BOLD}omp${C_RESET} (oh-my-pi): install omp, then re-run this installer to add the kiro models.
EOF
  fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  printf '%s\n' "${C_BOLD}kiro-proxy installer${C_RESET}  ${C_DIM}($OS/$ARCH → $DIR)${C_RESET}"

  resolve_credentials
  write_env
  if [ "$NO_START" = 1 ]; then
    ok "Configured. Skipping install/start (--no-start)."
    install_omp_plugin
    summary
    return
  fi

  RUN_METHOD=""
  case "$METHOD" in
    docker) install_docker || die "Docker install failed. Try --method source, or check that Docker can reach ghcr.io." ;;
    source) install_source || die "Source build failed. Install Go and git, or use --method docker." ;;
    auto)
      install_docker || install_source || die "No install method worked.
Need either a running Docker (able to pull $IMAGE) or a Go toolchain + git.
Install one and re-run, or pass --method explicitly." ;;
    *) die "unknown --method: $METHOD (use auto|docker|source)" ;;
  esac

  log "Waiting for the proxy to become healthy…"
  if wait_health; then ok "Healthy at http://localhost:$PORT/health"; else
    warn "Health check timed out. Check logs:"
    case "${RUN_METHOD:-}" in
      docker) warn "  docker logs kiro-proxy" ;;
      source) warn "  tail -n 50 $DIR/kiro-proxy.log" ;;
    esac
    if [ "$ASSIST" = 1 ]; then
      kiro_assist "kiro-proxy started but its /health endpoint on port $PORT isn't responding. Common causes and fixes for a Kiro auth/proxy gateway?"
    fi
  fi

  install_omp_plugin
  summary
}

main "$@"
