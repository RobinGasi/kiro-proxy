# omp-kiro

Kiro's models inside [oh-my-pi](https://github.com/oh-my-pi), driven by **omp's own agent loop** — omp's tools,
skills, rules, approval gate, checkpointing, subagents and system prompt, with Kiro supplying the inference.

```text
omp agent loop  ->  local kiro-proxy (/v1/chat/completions)  ->  Kiro backend
```

No AWS Bedrock adapter, no vendor SDK, no cloud API key. This is a model provider, not a delegation bridge: nothing
is handed off to a second agent, so the full omp toolchain stays in play on every turn.

## Install

The plugin lives in the [kiro-proxy](https://github.com/RobinGasi/kiro-proxy) repo. Its
[installer](https://github.com/RobinGasi/kiro-proxy#one-command-install-recommended) sets up the proxy and,
when `omp` is on your `PATH`, links this plugin too:

```sh
curl -fsSL https://raw.githubusercontent.com/RobinGasi/kiro-proxy/main/install.sh | bash
```

Developing on it instead? Link your clone, so edits in the working tree are live:

```sh
git clone https://github.com/RobinGasi/kiro-proxy.git
omp plugin uninstall omp-kiro   # only if a copy is installed from git/npm — link cannot replace it
omp plugin link ./kiro-proxy/omp-kiro
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
the catalog tracks whatever the proxy currently serves instead of a hardcoded list. When the proxy is down, errors, or
lists nothing, the fetch fails rather than substituting a stand-in list: omp keeps a returned list for 24 h but retries
a failed fetch within minutes, so the kiro models appear shortly after the proxy comes up. To refetch right away (e.g.
after Kiro adds a model), run `omp models refresh`.

**Token limits come from Kiro.** The proxy reads each model's `tokenLimits` from Kiro's own `ListAvailableModels` and
relays them on `/v1/models` as `context_window` and `max_output_tokens` — a private extension to the OpenAI shape,
omitted when no limit could be established. So GPT-5.6 Sol advertises the
[272K window Kiro documents](https://kiro.dev/changelog/models/gpt-5-6/) with a 128K output ceiling, and a model
Kiro re-tiers is picked up on the next refresh without a plugin change.

When a field is absent — a limit the proxy could not establish — `buildModel`
falls back to a table of the limits verified against Kiro's catalog, then to a conservative default (200k context,
8,192 output) for an id it has never seen. The `auto-kiro` router stays on that conservative default on purpose: its
backing model, and therefore its window, cannot be known ahead of time.

Metadata that is a property of the family rather than the catalog stays local:

- **Thinking** maps to `reasoning_effort`, which the proxy converts to a thinking budget as a percentage of max output
  tokens (`internal/api/openai/convert.go:reasoningEffortBudget`). Ladder `minimal, low, medium, high, xhigh`; `max`
  folds onto `xhigh`.
- **Modality**: Claude ids accept images, the rest are text-only.
- **Cost is reported as 0**: Kiro bills credits, not tokens — see [Credits](#credits).

Discovery is authoritative: omp persists the definition this extension returns, so a metadata fix has to land here (or
in the proxy) rather than being merged in from another provider's reference catalog.

## Credits

Because the billing unit is credits, omp's cost and token segments read `0` on every kiro turn. The status line
carries the credit pool instead, refreshed at session start and after every turn while a `kiro` model is active:

```text
credits 194/10,000
```

`/kiro-credits` forces a fresh read and prints the full breakdown:

```text
Kiro credits: 194 / 10,000 used · 9,806 left (98.1% free)
```

Both read the proxy's `GET /v1/usage`. Upstream reads are throttled to one per 15 s and coalesced, so a burst of
short turns cannot hammer Kiro's usage API through the proxy. A failed read keeps the last good value in the status
line — glanceable, never flickering — and `/kiro-credits` is where the reason is reported.

## Configuration

Environment variables, or `~/.omp/agent/.env` (profile-aware — the host's own `getAgentDir()` is used):

| Variable             | Default                                    | Meaning                          |
| -------------------- | ------------------------------------------ | --------------------------------- |
| `KIRO_PROXY_URL`     | `http://127.0.0.1:8442`                    | Proxy base; `/v1` is appended    |
| `KIRO_PROXY_API_KEY` | `PROXY_API_KEY`, then `~/.kiro-proxy/.env` | Bearer token                     |

Requires [`kiro-proxy`](https://github.com/RobinGasi/kiro-proxy) to be running (Docker container `kiro-proxy`, or
`~/.kiro-proxy/start.sh` for a source install).

The key is handed to omp as an **env var name**, not as the secret. omp resolves `apiKey` through its shared config
resolver (`!cmd` runs a shell command, otherwise env lookup, otherwise literal), so passing a name means a key that
happens to start with `!` can never be mistaken for a command.

## Layout

- `src/provider.ts` — extension factory: `registerProvider` for the `kiro` provider
- `src/proxy.ts` — endpoint/key resolution, live catalog discovery, model metadata and limit fallbacks
- `src/credits.ts` — credit balance cache, status-line readout, `/kiro-credits`

The package imports `@oh-my-pi/pi-coding-agent` **type-only**; the single host value it needs (`getAgentDir`) comes
through `pi.pi`, so a linked plugin never has to resolve the host package at runtime.
