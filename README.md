# kiro-proxy

An OpenAI- and Anthropic-compatible API gateway in front of **Kiro / Amazon Q
Developer / AWS CodeWhisperer**, written in Go. It lets tools and SDKs that speak
the OpenAI (`/v1/chat/completions`) or Anthropic (`/v1/messages`) protocols talk
to Kiro using credentials from **your own** Kiro / Amazon Q / AWS account.

**📚 For educational purposes only.** kiro-proxy is published for learning and
interoperability research — personal, local use with your own account. Use it at
your own risk and in accordance with the terms of any third-party services it
interacts with.

## Features

- **Two API surfaces:** OpenAI-compatible (`/v1/chat/completions`, `/v1/models`)
  and Anthropic-compatible (`/v1/messages`, `/v1/messages/count_tokens`), both
  with SSE streaming.
- **Robust auth / SSO:** Kiro Desktop refresh tokens **and** AWS SSO OIDC
  (kiro-cli / IAM Identity Center / Builder ID / corporate), from a refresh
  token, a JSON credentials file (incl. Enterprise `clientIdHash`), or the
  kiro-cli SQLite database (pure-Go, no CGO).
- **Newer Claude models just work:** model IDs are passed through (gateway, not
  gatekeeper), context-window suffixes like `claude-opus-4-8[1m]` are stripped,
  and inline `role:"system"` messages from newer Claude Code clients are merged
  into the system prompt instead of being rejected with HTTP 422.
- **Kiro credits as usage:** per-request credit metering surfaced in the `usage`
  object, plus a usage endpoint backed by Kiro's usage limits.
- **Multi-account failover** with a circuit breaker (optional).
- Extended-thinking parsing, vision, tool/function calling, web search,
  truncation recovery, payload-size guards, configurable debug logging.

## Quick start

### One-command install (recommended)

[`install.sh`](./install.sh) detects **how you log in** to Kiro — AWS IAM
Identity Center / Builder ID / social login (via the kiro-cli database), a Kiro
IDE credentials file, or a raw refresh token — writes `~/.kiro-proxy/.env`,
installs the proxy (**Docker** or **built from source**, auto-chosen), starts it,
and prints how to point Claude Code / Codex at it. If [omp](https://github.com/can1357/oh-my-pi)
is installed, it also sets up the bundled [omp-kiro](./omp-kiro) plugin, so Kiro
models show up in omp as `kiro/…`. Re-running it is safe: unchanged settings,
containers, binaries and plugin state are left alone, and settings you add to
`~/.kiro-proxy/.env` are kept.

```bash
curl -fsSL https://raw.githubusercontent.com/RobinGasi/kiro-proxy/main/install.sh | bash
```

If no credentials are found and `kiro-cli` is installed, the installer offers to
run `kiro-cli login` first (Builder ID / IdC-SSO / social). Pass flags after
`| bash -s --`, e.g. `... | bash -s -- --port 9000 --method docker`:

| Flag | Purpose |
|------|---------|
| `--port N` | Port to expose (default `8442`) |
| `--method docker\|source` | Force an install method (default: auto) |
| `--refresh-token TOK` / `--profile-arn ARN` | Use a raw refresh token as the source |
| `--assist` | If setup stalls, ask the kiro-cli AI for help |
| `-y`, `--yes` | Non-interactive; accept defaults |
| `--no-start` | Configure only; don't start |
| `--no-omp` | Don't set up the omp-kiro plugin |
| `--help` | Full list of flags and env-var overrides |

The proxy lands in `~/.kiro-proxy` (config, logs, and `start.sh`/`stop.sh` for
the source method; `docker … kiro-proxy` for the Docker method).

### Manual

```bash
cp .env.example .env        # then fill in your credentials + PROXY_API_KEY
go run ./cmd/kiro-proxy --port 8442
# or build a static binary:
go build -o kiro-proxy ./cmd/kiro-proxy && ./kiro-proxy
```

### Docker

```bash
docker compose up -d
# or:
docker run -p 8442:8442 --env-file .env \
  -v kiro-proxy-state:/state -e ACCOUNTS_STATE_FILE=/state/state.json \
  ghcr.io/robingasi/kiro-proxy:latest
```

## Authentication

Clients authenticate to the proxy with `PROXY_API_KEY` (multiple keys allowed,
comma/space separated):

- OpenAI style: `Authorization: Bearer <PROXY_API_KEY>`
- Anthropic style: `x-api-key: <PROXY_API_KEY>`

Pick **one** Kiro credential source in `.env`: `REFRESH_TOKEN` (+ `PROFILE_ARN`),
`KIRO_CREDS_FILE`, or `KIRO_CLI_DB_FILE`. See [`.env.example`](./.env.example)
for every option, and [`credentials.json.example`](./credentials.json.example)
for multi-account mode.

> `PROFILE_ARN` is **required** for most accounts on the current
> `runtime.kiro.dev` endpoint. The proxy resolves it automatically from the
> kiro-cli SQLite `state` table or the JSON credentials when present.

## Using with coding agents

Point any OpenAI- or Anthropic-compatible client at the proxy. Start it first
(`go run ./cmd/kiro-proxy --port 8442`), then configure your agent. Use a model
ID the proxy actually serves — list them with:

```bash
curl -s http://localhost:8442/v1/models -H "Authorization: Bearer <PROXY_API_KEY>"
```

### Claude Code (Anthropic-compatible)

Claude Code talks to the `/v1/messages` surface. Point it at the proxy with
environment variables (`ANTHROPIC_API_KEY` here is your **proxy** key, not a real
Anthropic key):

```bash
export ANTHROPIC_BASE_URL=http://localhost:8442
export ANTHROPIC_API_KEY=<PROXY_API_KEY>
export ANTHROPIC_MODEL='opus[1m]'                 # newest Opus; [1m] = 1M context
export ANTHROPIC_SMALL_FAST_MODEL=haiku             # background / small tasks
claude
```

Or per-session without exporting:

```bash
ANTHROPIC_BASE_URL=http://localhost:8442 ANTHROPIC_API_KEY=<PROXY_API_KEY> claude
```

> If you are logged into Claude Code with a subscription/OAuth session, log out
> (or run in a clean shell) so it uses the env vars above instead of your account.

### Codex CLI (OpenAI-compatible)

Codex talks to `/v1/chat/completions`. Add a custom provider to
`~/.codex/config.toml`:

```toml
model = "opus"                     # family name (→ newest Opus) or any id from GET /v1/models
model_provider = "kiro"

[model_providers.kiro]
name = "kiro-proxy"
base_url = "http://localhost:8442/v1"
env_key = "PROXY_API_KEY"            # Codex reads the key from this env var
wire_api = "chat"
```

Then run with the key in the environment:

```bash
export PROXY_API_KEY=<PROXY_API_KEY>
codex
```

### omp (oh-my-pi)

The [omp-kiro](./omp-kiro) plugin in this repo registers a `kiro` provider
backed by this proxy. The installer links it automatically when `omp` is
present (into `~/.kiro-proxy/omp-kiro`); to do it by hand from a clone:

```bash
omp plugin link ./omp-kiro
omp --model kiro/claude-opus-5 "…"
```

It reads `PROXY_API_KEY` from `~/.kiro-proxy/.env` and defaults to
`http://127.0.0.1:8442`; set `KIRO_PROXY_URL` / `KIRO_PROXY_API_KEY` in
`~/.omp/agent/.env` otherwise. Restart omp after installing.

> If you changed `SERVER_HOST` / `SERVER_PORT`, adjust the URLs above to match.

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| GET  | `/` | Health check |
| GET  | `/health` | Detailed health |
| GET  | `/v1/models` | List models (live catalog ∪ fallback seed) |
| POST | `/v1/chat/completions` | OpenAI chat (streaming + non-streaming) |
| POST | `/v1/messages` | Anthropic messages (streaming + non-streaming) |
| POST | `/v1/messages/count_tokens` | Anthropic token count |
| GET  | `/v1/usage` | Kiro credit / usage limits |

## Configuration

All settings are environment variables (loaded from `.env` if present). See
[`.env.example`](./.env.example) for the full annotated list — server host/port,
proxy keys, credential sources, regions, `VPN_PROXY_URL` (HTTP/SOCKS5), logging,
debug mode, streaming timeouts, fake-reasoning, payload guard, web search, and
multi-account / circuit-breaker tuning.

Zero-byte upstream responses are not retried. They open a persisted, model-scoped
cooldown (`MODEL_RESTRICTION_COOLDOWN_SECONDS`, default 3600) and return HTTP 503
with `Retry-After`; other models remain available. This lets compatible clients
fall back while preventing retry loops from reaching the paused Kiro model.

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

Layout (standard Go project structure):

- `cmd/kiro-proxy` — entry point
- `internal/config` — configuration
- `internal/auth` — token lifecycle, SSO (Kiro Desktop + AWS SSO OIDC)
- `internal/account` — multi-account failover
- `internal/upstream` — Kiro HTTP client + shared request builder
- `internal/model`, `internal/modelcache` — model resolution + dynamic catalog
- `internal/converter`, `internal/api/{anthropic,openai}` — format conversion + routes
- `internal/awsstream`, `internal/streaming`, `internal/thinking` — SSE parsing
- `internal/tokenizer`, `internal/truncation`, `internal/payloadguard`,
  `internal/mcp`, `internal/usage`, `internal/transport`, `internal/debuglog`,
  `internal/util`

## Releases

Pushes to `main` run the [release workflow](.github/workflows/release.yml):
[`semantic-release`](https://github.com/semantic-release/semantic-release)
analyzes the Conventional Commits and computes + tags the next version, then
[GoReleaser](https://goreleaser.com) builds the cross-platform binaries, creates
the GitHub Release (with notes and checksums), and publishes the multi-arch image
to `ghcr.io/robingasi/kiro-proxy` (`<version>`, `<major>.<minor>`, `latest`).
Public install notes:
<https://gist.github.com/RobinGasi/c6ee980e39d169fe34b7786d6ef68822>.

## License

GNU AGPL-3.0 — see [`LICENSE`](./LICENSE). Running a modified version as a network
service obliges you to offer its source to users. Provided with **no warranty**.
