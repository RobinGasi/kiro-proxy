# Kiro Claude Opus 5.5 behavior

**Research date:** 2026-10-01  
**Scope:** Public, maintained GitHub source/issues/PRs and Kiro's first-party model documentation. This note distinguishes evidence that names Opus 5.5 from generic Kiro/Claude behavior. It does not claim that an Anthropic/Bedrock observation is a Kiro-runtime observation unless the source says so.

## Executive summary

* Kiro's first-party announcement says Claude Opus 5.5 became available across IDE, CLI, Crew, and Web on 2026-09-28, with experimental regional rollout, a 1M context window, and a 2x credit multiplier. It also explicitly says some higher-risk requests are blocked and recommends switching models for false positives. [Kiro announcement](https://kiro.dev/blog/opus-5-5/)
* The strongest Opus-5.5-specific request-field evidence is external Claude API/adapter evidence: Opus 5.5 rejects `thinking: {"type":"disabled"}` and requires adaptive thinking plus `output_config.effort`; an independently maintained LiteLLM issue reports signature-only thinking (`thinking: ""`, signature present) for Opus 5.5 and fixes streaming preservation. These are not proof that Kiro's private runtime emits the same fields, but they justify catalog-schema-driven request construction and preservation of signed/empty reasoning. [Hermes #120069](https://github.com/NousResearch/hermes-agent/issues/120069), [LiteLLM #42871](https://github.com/BerriAI/litellm/issues/42871)
* The Kiro event protocol is fragmented by design: maintained parsers accept a `toolUseEvent` carrying any subset of `name`, `toolUseId`, `input`, and `stop`, with later frames carrying only input or stop. [pi-provider-kiro `event-parser.ts`](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/event-parser.ts), [OpenCodex `kiro-events.ts`](https://raw.githubusercontent.com/lidge-jun/opencodex/main/src/adapters/kiro-events.ts)
* Empty/malformed tool calls are real compatibility failures. A Kiro-specific report documents repeated 400s after translated `tool_result` turns due to empty serialized content and unstable/mismatched IDs; a Kiro-compatible Rust proxy issue records Claude Opus 4.5 `tool_use` blocks with `{}` and later fragmented `partial_json`; and a separate Bedrock report documents empty streamed arguments when object/snapshot input deltas are dropped. [OmniRoute #2446](https://github.com/diegosouzapw/OmniRoute/issues/2446), [kiro.rs #22](https://github.com/hank9999/kiro.rs/issues/22), [OmniRoute #14668](https://github.com/diegosouzapw/OmniRoute/issues/14668)
* No public source found during this research proves that Opus 5.5 specifically produces Kiro `toolUseEvent` fragmentation, empty `{}` arguments, or reasoning-only Kiro EOFs. Those remain hypotheses for this proxy, supported by generic Kiro behavior and Opus 5.5's independently observed signed-thinking shape—not established model-specific Kiro facts.

## Model identifiers and request fields

### Direct Opus 5.5 evidence

Kiro's official announcement names the product model “Claude Opus 5.5” but does not publish a wire `modelId` or catalog JSON schema. [Kiro announcement](https://kiro.dev/blog/opus-5-5/)

A maintained Hermes issue records the API identifier as `claude-opus-5-5` and notes that the dotted spelling `claude-opus-5.5` also needs recognition. The reported 400 is explicit: `thinking.type.disabled` is unsupported; the model requires `thinking.type.adaptive` and `output_config.effort`. The issue's maintainer comment says the fix adds both Opus spellings to the mandatory-thinking list. This is direct Opus-5.5 evidence, but it concerns the Anthropic adapter rather than Kiro's private request schema. [Hermes #120069, reproduction and maintainer fix](https://github.com/NousResearch/hermes-agent/issues/120069)

Kiro's own availability page is first-party model documentation, but its reader output exposes only metadata/title, not the model table or a machine-readable catalog schema. [Kiro available models](https://kiro.dev/docs/models/available-models/)

### Kiro catalog/schema evidence

`pi-provider-kiro` models retain `additionalModelRequestFieldsSchema` from Kiro management discovery and derive effort fields from the advertised JSON-schema properties. Its request builder supports either `{ reasoning: { effort } }`, or Claude-style `{ output_config: { effort }, thinking: { type: "adaptive", display?: "summarized" } }`; the schema's `enum` is used rather than assuming a fixed model list. [Model metadata](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/models.ts), [effort derivation/request builder](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/effort.ts)

That implementation's bootstrap list predates Opus 5.5 and contains no `claude-opus-5-5` entry; therefore it is evidence for the catalog/schema mechanism, not evidence that this repository currently supports Opus 5.5. Its explicit comment says catalog discovery overwrites bootstrap metadata. [Bootstrap models](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/models.ts)

The `kiro.rs` project has an open request to add Claude Opus 5.5 after it entered Kiro, but the issue contains no identifier, schema, or implementation. [kiro.rs #200](https://github.com/hank9999/kiro.rs/issues/200)

**Actionable implication:** resolve the authenticated Kiro catalog first; preserve the exact advertised ID and `additionalModelRequestFieldsSchema`; select `reasoning` versus `output_config` from schema properties and enum values. Do not hard-code the Anthropic spelling or send `thinking.type=disabled` merely because a client disabled reasoning. This is an implementation recommendation based on the Kiro adapter schema and direct Opus-5.5 API evidence, not a claim that Kiro's catalog must use one spelling.

## Fragmented tool events and validation

### What maintained Kiro implementations observe

`pi-provider-kiro` documents the generated Smithy `ChatResponseStream` union and routes by `:event-type`, not by payload shape. Its parser explicitly models a logical tool call as fragmented: a first event can carry `name` and `toolUseId`, continuation events can carry only `input`, and the final event can carry only `stop`. It converts string input directly, serializes non-empty object input, and collapses an empty object to an empty string. [event union and `parseToolUse`](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/event-parser.ts)

OpenCodex uses the same Kiro event union shape (`name?`, `toolUseId?`, `input?`, `stop?`) and parses optional fields with type validation. Its stream state keys open calls by ID, preserves arrival order for parallel calls, rekeys a locally generated ID if the upstream ID arrives later, and flushes open calls at EOF only after `finishToolUse` validation. The parser itself accepts an empty or partial event; the state machine decides whether the call is complete. [OpenCodex event parser](https://raw.githubusercontent.com/lidge-jun/opencodex/main/src/adapters/kiro-events.ts), [OpenCodex tool state/EOF handling](https://github.com/lidge-jun/opencodex/blob/main/src/adapters/kiro/stream.ts)

Kiro-Go's parser also tracks open calls by `toolUseId`, keeps arrival order, supports rekeying a generated ID, and says explicitly that an incomplete tool at EOF must cause retry rather than delivering missing arguments. [Kiro-Go parser and `pendingToolUses`](https://github.com/Quorinex/Kiro-Go/blob/main/proxy/kiro.go#L690-L950)

### Concrete malformed/empty reports

* `kiro.rs` issue #22 includes captured Anthropic SSE for `claude-opus-4-5-20251101`: `content_block_start` has a `tool_use` with `input:{}`, followed in another capture by many `input_json_delta` fragments (`partial_json`) and then `tool_use` stop. The issue attributes the repeated Write failure to a Claude Code bug, so it is generic Claude/Kiro-proxy evidence, **not Opus 5.5 evidence**. [kiro.rs #22](https://github.com/hank9999/kiro.rs/issues/22)
* OmniRoute #2446 reports a Kiro follow-up 400 whenever history contains a `tool_result`. Its root-cause analysis identifies three translation hazards: hard-coded success status, structured/non-text results becoming `[{text:""}]`, and a generated assistant ID not matching the later `tool_use_id`; the maintainer confirms the empty-text serializer defect and says it was fixed in #2465. This is direct Kiro-backend evidence, but no Opus 5.5 model is named. [OmniRoute #2446](https://github.com/diegosouzapw/OmniRoute/issues/2446)
* OmniRoute #14668 reports a separate Bedrock `ConverseStream` bug: tool arguments become `""` or `{}` when the executor forwards only string `toolUse.input` deltas and drops object/snapshot-shaped deltas. It was reproduced with Bedrock Claude Opus 5 (not Opus 5.5) and GPT-6 Astra, so it is generic Bedrock/translator evidence, not Kiro or Opus 5.5 evidence. The issue explicitly recommends accumulating arbitrary input shapes and deduplicating snapshots. [OmniRoute #14668](https://github.com/diegosouzapw/OmniRoute/issues/14668)

### Comparison and required proxy invariants

| Concern | pi-provider-kiro | OpenCodex | Kiro-Go | Proxy implication |
|---|---|---|---|---|
| Frame routing | Smithy `:event-type` union | Smithy event type; unknown ignored | AWS EventStream `:event-type` and CRC validation | Route by event header; do not shape-scan known frames. |
| Missing ID/name | Parser permits optional fields; stream-level state required | Optional fields accepted; state owns completeness | Generated ID may be rekeyed when real ID arrives | Do not reject input-only/stop-only continuations; reject an unfinishable call before delivery. |
| Same-ID mismatch | Public parser is permissive | State is ID-keyed; completion validates | ID-keyed state; rekey only generated IDs | Keep one state per ID; treat a non-generated same-ID name change as malformed, not a new call. |
| Duplicate terminal frames | No explicit public duplicate policy in parser | Completion/state logic guards open-call lifecycle | `remove`/flush lifecycle prevents normal duplicate emission | Make `stop` idempotent only for the same call; do not emit a second tool call or replay side effects. |
| Input snapshots vs deltas | Serializes object input; does not claim snapshot dedupe | Parser exposes input; stream assembly owns it | Appends input into per-ID buffer | Accept strings and object snapshots; detect already-contained snapshots before appending, otherwise concatenate JSON fragments. |
| EOF with open calls | Stream implementation should not expose incomplete state | `flushAll` validates and errors on incomplete tool | `flushAll` explicitly retries rather than delivering missing args | Treat clean EOF with open/invalid tool input as incomplete, never a successful empty tool call. |

These invariants are a synthesis of implementation behavior and are therefore recommendations, not a published Kiro specification.

## Reasoning-only, metadata-only, and retry evidence

### Direct Opus 5.5 evidence (non-Kiro transport)

LiteLLM #42871 says streaming `/v1/responses` dropped signature-only thinking for Claude Fable 5.1, Opus 5.5, and Bedrock adaptive thinking. The observed shape is a thinking block with empty text and a signature; before the fix, the output contained only a message and the signed reasoning could not be replayed on the next turn. The merged fix opens a reasoning item whenever signed/redacted thinking arrives and preserves empty signed blocks. This is direct Opus-5.5 evidence for a metadata/signature-only reasoning shape, not evidence of a Kiro `reasoningContentEvent` EOF. [LiteLLM #42871](https://github.com/BerriAI/litellm/issues/42871)

Hermes #120069 reports a different Opus-5.5-specific failure: reasoning-disabled side calls (title generation, compression fast lane, `/reasoning none`) get non-retryable 400s because the adapter sends `thinking.type=disabled`. Main reasoning-enabled calls succeed. The issue was fixed by omitting the disabled form for Opus 5.5. This is a request-validation failure, not a reasoning-only completion. [Hermes #120069](https://github.com/NousResearch/hermes-agent/issues/120069)

Kiro's official announcement says Opus 5.5 has safeguards comparable to another Claude model and that some higher-risk requests are blocked; it recommends switching models after a false positive. This supports a model-specific fallback policy for explicit policy errors, but does not establish that an unexplained Kiro EOF is a safeguard decision. [Kiro announcement](https://kiro.dev/blog/opus-5-5/)

### Generic Kiro evidence

OpenCodex treats a Kiro stream with reasoning but no validated final answer as `reasoning_only_kiro_fallback`, and a stream with neither text nor reasoning as `empty_kiro_fallback`; it retries only when no output was emitted to the client. Its comments also distinguish native `END_TURN` from a validated final answer and reject `TOOL_USE` without an emitted call. [OpenCodex stream completion/retry](https://github.com/lidge-jun/opencodex/blob/main/src/adapters/kiro/stream.ts)

Kiro-Go classifies clean EOF with no assistant/reasoning/tool output as `errEmptyKiroStream`, retries pre-callback failures with bounded backoff, and refuses to deliver incomplete open tools. Its parser counts reasoning text as output, so it does **not** prove that reasoning-only means a completed user-visible answer. [Kiro-Go stream parser/retry](https://github.com/Quorinex/Kiro-Go/blob/main/proxy/kiro.go#L560-L950)

OpenCodex issue #519 reports a Kiro 200 response containing only heartbeat events for about 60 seconds, then a socket close with zero output tokens. The issue says replay was safe because nothing had been relayed, while the original implementation incorrectly surfaced a terminal 502. This is direct Kiro transport evidence, but it is not Opus-5.5-specific. [OpenCodex #519](https://github.com/lidge-jun/opencodex/issues/519)

**Actionable implication:** keep reasoning/signatures separate from final text; preserve signature-only reasoning if the client contract supports replay; classify metadata/context-only and reasoning-only EOF separately; retry only before any client-visible text, reasoning, or tool call has been emitted. Do not fabricate an answer or normal stop from metadata, a signature, or `END_TURN` alone. Do not retry explicit policy/refusal or malformed-output errors unchanged. These rules follow the three maintained Kiro implementations above; Opus 5.5 evidence increases the need for them but does not prove Kiro's exact failure mode.

## Recommended acceptance tests for this proxy

1. **Catalog/request:** with a fixture catalog entry whose ID is `claude-opus-5-5` or `claude-opus-5.5`, preserve the exact ID; derive the advertised `output_config.effort`/`thinking.adaptive` shape; never send `thinking.type=disabled` for Opus 5.5. Source basis: [Hermes #120069](https://github.com/NousResearch/hermes-agent/issues/120069), [pi effort builder](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/effort.ts).
2. **Fragment assembly:** feed a start `{name,toolUseId}`, repeated input-only frames, an object snapshot, a duplicate snapshot, and a stop frame. Emit exactly one call with one stable ID and valid complete JSON. Source basis: [pi parser](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/event-parser.ts), [OpenCodex state](https://github.com/lidge-jun/opencodex/blob/main/src/adapters/kiro/stream.ts), [OmniRoute #14668](https://github.com/diegosouzapw/OmniRoute/issues/14668).
3. **Malformed state:** reject or classify as incomplete a missing-ID call, same-ID name mutation, invalid JSON, duplicate terminal frame, and EOF with an open call; never turn `{}` into a successful tool invocation unless the upstream explicitly completed a genuinely empty schema. Source basis: [Kiro-Go EOF handling](https://github.com/Quorinex/Kiro-Go/blob/main/proxy/kiro.go#L690-L950), [kiro.rs #22](https://github.com/hank9999/kiro.rs/issues/22).
4. **Completion:** feed context/metadata only, reasoning/signature only, and heartbeat-only EOFs. Retry only before forwarding output; after forwarding, return an incomplete/terminal error rather than replaying a tool or duplicating text. Source basis: [OpenCodex stream retry](https://github.com/lidge-jun/opencodex/blob/main/src/adapters/kiro/stream.ts), [OpenCodex #519](https://github.com/lidge-jun/opencodex/issues/519), [LiteLLM #42871](https://github.com/BerriAI/litellm/issues/42871).

## Source boundary

Compared maintained Kiro implementations: [mikeyobrien/pi-provider-kiro](https://github.com/mikeyobrien/pi-provider-kiro), [lidge-jun/OpenCodex](https://github.com/lidge-jun/opencodex), and [Quorinex/Kiro-Go](https://github.com/Quorinex/Kiro-Go). Additional maintained Kiro-compatible evidence: [hank9999/kiro.rs](https://github.com/hank9999/kiro.rs), [diegosouzapw/OmniRoute](https://github.com/diegosouzapw/OmniRoute), and [decolua/9router](https://github.com/decolua/9router). First-party Kiro evidence: [Opus 5.5 announcement](https://kiro.dev/blog/opus-5-5/) and [available models](https://kiro.dev/docs/models/available-models/). Opus-5.5 behavior evidence outside Kiro is explicitly labeled above.
