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
# `kiro-cli login` and (best-effort) to ask the kiro-cli AI for help.
#
# Everything is overridable via flags or env vars — run with --help.
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults (override with env vars or flags)
# ---------------------------------------------------------------------------
REPO="${KIRO_PROXY_REPO:-RobinGasi/kiro-proxy}"
REF="${KIRO_PROXY_REF:-main}"
DIR="${KIRO_PROXY_DIR:-$HOME/.kiro-proxy}"
PORT="${KIRO_PROXY_PORT:-8442}"
REGION="${KIRO_REGION:-us-east-1}"
METHOD="${KIRO_PROXY_METHOD:-auto}"     # auto | docker | source
ASSUME_YES=0
ASSIST=0
NO_START=0
REFRESH_TOKEN="${REFRESH_TOKEN:-}"
PROFILE_ARN="${PROFILE_ARN:-}"

# Launcher alias: a small command placed on PATH that starts Claude Code with the
# proxy env vars already set. ALIAS_NAME non-empty implies "create it".
ALIAS_NAME="${KIRO_PROXY_ALIAS:-}"   # e.g. "cac"; empty → prompt interactively
ALIAS_DEFAULT="cac"                  # suggested name when prompting
NO_ALIAS=0                           # --no-alias skips launcher creation entirely
LAUNCHER_NAME=""                     # filled in once a launcher is written
LAUNCHER_PATH=""

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
  --alias NAME          Install a PATH command NAME that launches Claude Code
                        through the proxy (default prompt suggests '$ALIAS_DEFAULT')
  --no-alias            Don't offer to install a launcher command
  -y, --yes             Non-interactive; accept all defaults
  --no-start            Configure and install, but don't start the proxy
  -h, --help            Show this help

Env var overrides: KIRO_PROXY_DIR, KIRO_PROXY_PORT, KIRO_REGION,
KIRO_PROXY_METHOD, KIRO_PROXY_REPO, KIRO_PROXY_REF, REFRESH_TOKEN, PROFILE_ARN,
KIRO_PROXY_ALIAS.
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
    --alias) ALIAS_NAME="$2"; shift 2 ;;
    --no-alias) NO_ALIAS=1; shift ;;
    -y|--yes) ASSUME_YES=1; shift ;;
    --no-start) NO_START=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

OWNER="$(lower "${REPO%%/*}")"
IMAGE="ghcr.io/$OWNER/kiro-proxy:latest"

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

existing_key() {
  [ -f "$ENV_FILE" ] || return 0
  sed -n 's/^PROXY_API_KEY=//p' "$ENV_FILE" | head -n1
}

write_env() {
  mkdir -p "$DIR"
  PROXY_API_KEY="$(existing_key)"
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

  umask 077
  cat > "$ENV_FILE" <<EOF
# Generated by kiro-proxy install.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ).
# Full annotated reference: .env.example in the repository.
SERVER_HOST=0.0.0.0
SERVER_PORT=$PORT

# Clients authenticate to the proxy with this key.
PROXY_API_KEY=$PROXY_API_KEY

KIRO_REGION=$REGION

# --- credential source (auto-detected: ${CRED_KIND}$( [ -n "$CRED_LOGIN" ] && printf ' / %s' "$CRED_LOGIN" )) ---
$credblock
EOF
  chmod 600 "$ENV_FILE"
  ok "Wrote $ENV_FILE"
}

# ---------------------------------------------------------------------------
# Install method: docker
# ---------------------------------------------------------------------------
docker_available() { have docker && docker info >/dev/null 2>&1; }

ghcr_login() {
  # Private image: authenticate to GHCR using the gh token when available.
  have gh || return 0
  local user token
  token="$(gh auth token 2>/dev/null || true)"
  user="$(gh api user -q .login 2>/dev/null || true)"
  [ -n "$token" ] && [ -n "$user" ] || return 0
  printf '%s' "$token" | docker login ghcr.io -u "$user" --password-stdin >/dev/null 2>&1 \
    && ok "Logged in to ghcr.io as $user" || warn "GHCR login failed; pull may fail if the image is private."
}

install_docker() {
  docker_available || return 1
  log "Installing via Docker ($IMAGE)…"
  ghcr_login
  if ! docker pull "$IMAGE" >/dev/null 2>&1; then
    warn "Could not pull $IMAGE (private image not published yet, or not logged in)."
    return 1
  fi

  docker rm -f kiro-proxy >/dev/null 2>&1 || true

  # Build the argument list as an array so paths with spaces (e.g. macOS
  # "Library/Application Support/…") survive intact.
  # Map host:container to the same port and let the app bind $PORT inside (via
  # SERVER_PORT in the env-file), so any --port works — not just the default.
  local args=(-d --name kiro-proxy --restart unless-stopped
    -p "$PORT:$PORT" --env-file "$ENV_FILE")

  # Mount a local credential file (SQLite/JSON) read-only and point the proxy at it.
  if [ "$CRED_KIND" = "sqlite" ] || [ "$CRED_KIND" = "creds_file" ]; then
    local cdir cbase
    cdir="$(cd "$(dirname "$CRED_PATH")" && pwd)"
    cbase="$(basename "$CRED_PATH")"
    args+=(--mount "type=bind,source=$cdir,target=/creds,readonly")
    if [ "$CRED_KIND" = "sqlite" ]; then args+=(-e "KIRO_CLI_DB_FILE=/creds/$cbase")
    else args+=(-e "KIRO_CREDS_FILE=/creds/$cbase"); fi
  fi

  args+=("$IMAGE")

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
  if have gh; then
    gh repo clone "$REPO" "$dest" -- --depth 1 --branch "$REF" >/dev/null 2>&1 && return 0
    gh repo clone "$REPO" "$dest" >/dev/null 2>&1 && return 0
  fi
  git clone --depth 1 --branch "$REF" "https://github.com/$REPO.git" "$dest" >/dev/null 2>&1
}

install_source() {
  have go || { warn "Go toolchain not found; can't build from source."; return 1; }
  log "Building from source ($REPO@$REF)…"
  clone_source || { warn "Clone failed (private repo? install/auth 'gh' first)."; return 1; }
  ( cd "$DIR/src" && CGO_ENABLED=0 go build -trimpath -o "$DIR/kiro-proxy" ./cmd/kiro-proxy ) \
    || { warn "go build failed."; return 1; }
  ok "Built $DIR/kiro-proxy"

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
[ -f "$DIR/kiro-proxy.pid" ] && kill "\$(cat "$DIR/kiro-proxy.pid")" 2>/dev/null && rm -f "$DIR/kiro-proxy.pid" && echo stopped || echo "not running"
EOF
  chmod +x "$DIR/start.sh" "$DIR/stop.sh"

  if [ "$NO_START" != 1 ]; then
    "$DIR/start.sh"
  fi
  RUN_METHOD="source"
}

# ---------------------------------------------------------------------------
# Launcher command: a small script on PATH that starts Claude Code pre-wired to
# the proxy. Idempotent — re-running rewrites the same file in place.
# ---------------------------------------------------------------------------
LAUNCHER_MARKER="kiro-proxy-launcher"

# shell_rc echoes the login shell's rc file for PATH hints. For bash it prefers
# an existing ~/.bash_profile (which macOS login shells read instead of .bashrc).
shell_rc() {
  case "$(basename "${SHELL:-sh}")" in
    zsh)  printf '%s' "$HOME/.zshrc" ;;
    bash)
      if [ -f "$HOME/.bash_profile" ]; then printf '%s' "$HOME/.bash_profile"
      elif [ -f "$HOME/.bashrc" ]; then printf '%s' "$HOME/.bashrc"
      elif [ "$OS" = darwin ]; then printf '%s' "$HOME/.bash_profile"
      else printf '%s' "$HOME/.bashrc"; fi ;;
    *)    printf '%s' "$HOME/.profile" ;;
  esac
}

# launcher_is_ours PATH -> 0 when the file is one of our generated launchers.
launcher_is_ours() { [ -f "$1" ] && grep -q "$LAUNCHER_MARKER" "$1" 2>/dev/null; }

# name_free NAME -> 0 when NAME is unused, or already one of our launchers (so it
# can be safely updated). Non-zero when some other command owns the name.
name_free() {
  local existing
  existing="$(command -v "$1" 2>/dev/null || true)"
  [ -z "$existing" ] && return 0
  launcher_is_ours "$existing"
}

# valid_name NAME -> 0 for a sane command name (letter/underscore start).
valid_name() { printf '%s' "$1" | grep -Eq '^[A-Za-z_][A-Za-z0-9_-]*$'; }

# pick_bindir chooses a writable install directory, preferring one already on PATH.
pick_bindir() {
  local d
  for d in "$HOME/.local/bin" "$HOME/bin" "/usr/local/bin"; do
    case ":$PATH:" in *":$d:"*) [ -d "$d" ] && [ -w "$d" ] && { printf '%s' "$d"; return; } ;; esac
  done
  printf '%s' "$HOME/.local/bin"
}

# write_launcher NAME DIR — write (or overwrite) the launcher script. Returns
# non-zero (without aborting the installer) if DIR can't be written.
write_launcher() {
  local name="$1" dir="$2" target="$2/$1"
  if ! mkdir -p "$dir" 2>/dev/null || [ ! -w "$dir" ]; then
    warn "Can't write to $dir; skipping the launcher (set one up manually — see below)."
    return 1
  fi
  # The launcher reads the key/port from $ENV_FILE at runtime, so it keeps working
  # if either changes; the baked-in values are only a fallback.
  if ! cat > "$target" <<EOF
#!/usr/bin/env bash
# $LAUNCHER_MARKER — launches Claude Code through your local kiro-proxy.
# Generated by kiro-proxy install.sh; re-running the installer overwrites this.
set -eu

env_file="$ENV_FILE"
key="" port=""
if [ -f "\$env_file" ]; then
  key="\$(sed -n 's/^PROXY_API_KEY=//p' "\$env_file" | head -n1)"
  port="\$(sed -n 's/^SERVER_PORT=//p' "\$env_file" | head -n1)"
fi
: "\${port:=$PORT}"
: "\${key:=$PROXY_API_KEY}"

export ANTHROPIC_BASE_URL="http://localhost:\$port"
export ANTHROPIC_API_KEY="\$key"
# Bare family names — the proxy resolves them to the newest model in each family.
export ANTHROPIC_MODEL="\${ANTHROPIC_MODEL:-opus}"
export ANTHROPIC_SMALL_FAST_MODEL="\${ANTHROPIC_SMALL_FAST_MODEL:-haiku}"

# Opt out of all Claude Code analytics / telemetry / error reporting / auto-update.
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
export DISABLE_TELEMETRY=1
export DISABLE_ERROR_REPORTING=1
export DISABLE_AUTOUPDATER=1
export DISABLE_BUG_COMMAND=1
export DO_NOT_TRACK=1

if ! command -v claude >/dev/null 2>&1; then
  echo "kiro-proxy: 'claude' (Claude Code) is not on your PATH." >&2
  echo "Install it with: npm install -g @anthropic-ai/claude-code" >&2
  exit 127
fi
exec claude "\$@"
EOF
  then
    warn "Failed to write $target; skipping the launcher."
    return 1
  fi
  chmod +x "$target" 2>/dev/null || true
  LAUNCHER_NAME="$name"
  LAUNCHER_PATH="$target"
}

# find_existing_launcher — echoes the path of a launcher we previously installed
# (searching the dirs pick_bindir considers), or nothing. Lets re-runs refresh an
# existing launcher instead of prompting again.
find_existing_launcher() {
  local d f
  for d in "$HOME/.local/bin" "$HOME/bin" "/usr/local/bin"; do
    [ -d "$d" ] || continue
    for f in "$d"/*; do
      [ -f "$f" ] || continue
      if launcher_is_ours "$f"; then printf '%s' "$f"; return; fi
    done
  done
}

# maybe_add_to_path DIR — ensure DIR is on PATH. Interactively it offers to add
# the line to the shell rc file; non-interactively it only prints the instruction
# (it never edits dotfiles without an explicit prompt). Never aborts the caller.
maybe_add_to_path() {
  local d="$1" rc line
  case ":$PATH:" in *":$d:"*) return 0 ;; esac
  rc="$(shell_rc)"
  line="export PATH=\"$d:\$PATH\""
  if [ -f "$rc" ] && grep -Fq "$d" "$rc" 2>/dev/null; then
    warn "$d isn't on PATH in this shell, but $rc already adds it — open a new terminal."
    return 0
  fi
  # Only offer the automatic edit when we truly have an interactive terminal;
  # otherwise a no-TTY confirm would default to yes and silently edit dotfiles.
  if [ -n "$TTY" ] && [ "$ASSUME_YES" != 1 ] && confirm "  Add $d to your PATH in $rc? [Y/n] " "y"; then
    if printf '\n# Added by kiro-proxy install.sh\n%s\n' "$line" >> "$rc" 2>/dev/null; then
      ok "Updated $rc — run 'source $rc' or open a new terminal to pick up '$LAUNCHER_NAME'."
      return 0
    fi
    warn "Couldn't write $rc."
  fi
  warn "Add this to your shell config to use '$LAUNCHER_NAME': $line"
}

# prompt_alias_name — loop until a valid, free (or ours) name is entered; echoes
# the chosen name (Enter accepts the default $ALIAS_DEFAULT).
prompt_alias_name() {
  local name="" tries=0
  while [ "$tries" -lt 5 ]; do
    tries=$((tries + 1))
    name="$(ask "  Command name [$ALIAS_DEFAULT] (Enter to accept): " "$ALIAS_DEFAULT")"
    [ -z "$name" ] && return
    if ! valid_name "$name"; then
      warn "'$name' isn't a valid command name (letters, digits, _ and - only)."
    elif name_free "$name"; then
      printf '%s' "$name"; return
    else
      warn "'$name' is already a command on this system. Pick another."
    fi
  done
  warn "Too many attempts; skipping the launcher."
}

install_launcher() {
  [ "$NO_ALIAS" = 1 ] && return 0

  local name="$ALIAS_NAME" dir existing
  existing="$(find_existing_launcher)"

  # Idempotent re-run: a launcher we installed already exists and no specific name
  # was requested — refresh it in place, no prompt.
  if [ -z "$name" ] && [ -n "$existing" ]; then
    dir="$(dirname "$existing")"
    if write_launcher "$(basename "$existing")" "$dir"; then
      ok "Launcher '$LAUNCHER_NAME' already installed — refreshed ($LAUNCHER_PATH)."
      maybe_add_to_path "$dir" || true
    fi
    return 0
  fi

  if [ -n "$name" ]; then
    # Name supplied via flag/env: validate and check for collisions, don't loop.
    valid_name "$name" || { warn "Ignoring invalid --alias '$name'."; return 0; }
    if ! name_free "$name"; then
      warn "'$name' is already a command; not overwriting it. Choose another with --alias."
      return 0
    fi
  else
    # No explicit name: only prompt when we have an interactive terminal.
    { [ "$ASSUME_YES" = 1 ] || [ -z "$TTY" ]; } && return 0
    confirm "Create a '$ALIAS_DEFAULT'-style command to launch Claude Code via the proxy? [Y/n] " "y" || return 0
    name="$(prompt_alias_name)"
    [ -z "$name" ] && { warn "Skipping launcher."; return 0; }
  fi

  dir="$(pick_bindir)"
  # A launcher write failure must never abort the installer (the proxy is already
  # running by this point) — skip gracefully so summary() still prints.
  if write_launcher "$name" "$dir"; then
    ok "Installed launcher: $LAUNCHER_PATH"
    maybe_add_to_path "$dir" || true
  fi
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

summary() {
  printf '\n%s%s kiro-proxy is set up %s\n' "$C_GREEN$C_BOLD" "──" "$C_RESET"
  printf '  %sBase URL%s  http://localhost:%s\n' "$C_DIM" "$C_RESET" "$PORT"
  printf '  %sAPI key %s  %s\n' "$C_DIM" "$C_RESET" "$PROXY_API_KEY"
  printf '  %sConfig  %s  %s\n' "$C_DIM" "$C_RESET" "$ENV_FILE"
  case "${RUN_METHOD:-}" in
    docker) printf '  %sManage  %s  docker logs -f kiro-proxy | docker restart kiro-proxy | docker rm -f kiro-proxy\n' "$C_DIM" "$C_RESET" ;;
    source) printf '  %sManage  %s  %s/start.sh | %s/stop.sh | tail -f %s/kiro-proxy.log\n' "$C_DIM" "$C_RESET" "$DIR" "$DIR" "$DIR" ;;
  esac

  if [ -n "$LAUNCHER_NAME" ]; then
    printf '  %sLaunch  %s  %s%s%s   %s(%s)%s\n' \
      "$C_DIM" "$C_RESET" "$C_BOLD" "$LAUNCHER_NAME" "$C_RESET" "$C_DIM" "$LAUNCHER_PATH" "$C_RESET"
  fi

  cat <<EOF

${C_BOLD}List available models:${C_RESET}
  curl -s http://localhost:$PORT/v1/models -H "Authorization: Bearer $PROXY_API_KEY"

EOF

  if [ -n "$LAUNCHER_NAME" ]; then
    cat <<EOF
${C_BOLD}Claude Code${C_RESET} (Anthropic-compatible): just run
  $LAUNCHER_NAME

EOF
  else
    cat <<EOF
${C_BOLD}Claude Code${C_RESET} (Anthropic-compatible):
  export ANTHROPIC_BASE_URL=http://localhost:$PORT
  export ANTHROPIC_API_KEY=$PROXY_API_KEY
  export ANTHROPIC_MODEL=opus
  export ANTHROPIC_SMALL_FAST_MODEL=haiku
  claude

EOF
  fi

  cat <<EOF
${C_BOLD}Codex CLI${C_RESET} (OpenAI-compatible): add a provider with
  base_url = "http://localhost:$PORT/v1"  and  env_key = "PROXY_API_KEY"
  (see README → "Using with coding agents").
EOF
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
    install_launcher
    summary
    return
  fi

  RUN_METHOD=""
  case "$METHOD" in
    docker) install_docker || die "Docker install failed. Try --method source, or fix Docker/GHCR access." ;;
    source) install_source || die "Source build failed. Install Go and 'gh', or use --method docker." ;;
    auto)
      install_docker || install_source || die "No install method worked.
Need either a running Docker (with access to $IMAGE) or a Go toolchain + git/gh.
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

  install_launcher
  summary
}

main "$@"
