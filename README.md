# omp-kiro

Kiro's models inside [oh-my-pi](https://github.com/oh-my-pi), driven by **omp's own agent loop** — omp's tools,
skills, rules, approval gate, checkpointing, subagents and system prompt, with Kiro supplying the inference.

```text
omp agent loop  ->  local kiro-proxy (/v1/chat/completions)  ->  Kiro backend
```

No AWS Bedrock adapter, no vendor SDK, no cloud API key. This is a model provider, not a delegation bridge: nothing
is handed off to a second agent, so the full omp toolchain stays in play on every turn.

## Install

```sh
omp plugin link ~/projects/private/omp-kiro
```

Restart omp — extension modules are wired at session start, so `/reload-plugins` is not enough.

## Use

```sh
omp models                                   # kiro (17)
omp --model kiro/claude-opus-5 "…"
omp --model kiro/claude-sonnet-4.6:xhigh "…"
```

Make it the default in `~/.omp/agent/config.yml`:

```yaml
modelRoles:
  default: kiro/claude-opus-5:xhigh
```

## How the catalog works

Registered via `pi.registerProvider("kiro", …)` on the `openai-completions` wire API. The model list is fetched live
from the proxy's `/v1/models` through `fetchDynamicModels`, which rides omp's own SQLite model cache (24 h TTL), so
the catalog tracks whatever the proxy currently serves instead of a hardcoded list. A small seed list covers the case
where the proxy is down *and* the cache is cold.

Per-family metadata the bare id list cannot supply:

- **Thinking** maps to `reasoning_effort`, which the proxy converts to a thinking budget as a percentage of max output
  tokens (`internal/api/openai/convert.go:reasoningEffortBudget`). Ladder `minimal, low, medium, high, xhigh`; `max`
  folds onto `xhigh`.
- **Context windows** mirror the proxy's `ContextLimitFor` — 1M for Opus/Sonnet 4.x+, 200k for Haiku 4.5 and the
  `auto-kiro` router (whose backing model cannot be known ahead of time, so it stays conservative).
- **Cost is reported as 0**: Kiro bills credits, not tokens.

omp merges its own reference metadata for ids it already knows (`gpt-5.6-*`, `minimax-*`), so those rows show their
native effort ladders rather than the Claude one.

## Configuration

Environment variables, or `~/.omp/agent/.env` (profile-aware — the host's own `getAgentDir()` is used):

| Variable             | Default                                    | Meaning                          |
| -------------------- | ------------------------------------------ | --------------------------------- |
| `KIRO_PROXY_URL`     | `http://127.0.0.1:8442`                    | Proxy base; `/v1` is appended    |
| `KIRO_PROXY_API_KEY` | `PROXY_API_KEY`, then `~/.kiro-proxy/.env` | Bearer token                     |

Requires `kiro-proxy` to be running (`~/.kiro-proxy/start.sh`).

The key is handed to omp as an **env var name**, not as the secret. omp resolves `apiKey` through its shared config
resolver (`!cmd` runs a shell command, otherwise env lookup, otherwise literal), so passing a name means a key that
happens to start with `!` can never be mistaken for a command.

## Layout

- `src/provider.ts` — extension factory: `registerProvider` for the `kiro` provider
- `src/proxy.ts` — endpoint/key resolution, live model list, per-family model metadata

The package imports `@oh-my-pi/pi-coding-agent` **type-only**; the single host value it needs (`getAgentDir`) comes
through `pi.pi`, so a linked plugin never has to resolve the host package at runtime.
