# Kiro ACP migration tradeoffs

**Research date:** 2026-10-02  
**Scope:** A complete replacement of kiro-proxy's OpenAI/Anthropic provider surfaces with Kiro CLI's Agent Client Protocol (`kiro-cli acp`), compared against a hybrid where ACP is an optional backend or delegated agent. Opus 5.5 implementation claims use only repositories pushed after its 2026-09-22 release; older projects are architecture references only. Sources are the official ACP schema/catalog, Kiro's ACP documentation and issue tracker, Kiro CLI 2.27.0's locally observed initialization response, and current post-release implementations.

## Decision

Do **not** replace kiro-proxy completely with ACP. Keep the direct proxy as the default provider path and consider ACP as an optional backend for clients willing to accept Kiro CLI's session semantics.

ACP removes Kiro EventStream translation because the first-party CLI owns that wire. Current source proves it can still be adapted into a host-owned model loop: [`kiro-acp-ai-provider` 3.3.0](https://github.com/NachoFLizaur/kiro-acp-ai-provider/commit/1f1c8cd7ac1a0735381ddc36b08e11ba93703757), pushed 2026-10-01, relays application tool calls through an MCP/IPC bridge rather than executing them itself. That weakens the original "ACP always replaces the agent" claim. It does not remove semantic translation: system prompts, history rewrites, usage, sampling controls, tool-result images, concurrency, and errors still need adapter policy.

## What ACP changes

The official Agent Client Protocol connects a client to an autonomous agent and exposes sessions, prompts, thought chunks, tool calls, permission requests, and context updates. It is not itself an OpenAI or Anthropic model protocol. [Official repository](https://github.com/agentclientprotocol/agent-client-protocol), [protocol schema](https://agentclientprotocol.com/protocol/v1/schema)

Two current integration shapes exist. The local `pi-config/extensions/kiro-acp` extension delegates a complete task to Kiro. The post-release `kiro-acp-ai-provider` instead adapts ACP to Vercel AI SDK `LanguageModelV3` and relays host tools through an MCP bridge. Therefore ACP can support either delegation or a provider-like adapter, but the latter recreates many policy decisions currently implemented in kiro-proxy.

## Benefits

1. **Kiro owns its private wire contract.** Model discovery, native/adaptive thinking, signed reasoning, EventStream framing, fragmented tool calls, authentication, and internal continuation state stay inside the first-party CLI rather than being translated by this proxy.
2. **Native agent events.** ACP has structured agent messages, thought chunks, tool calls, permission requests, session modes/configuration, cancellation, and context-usage updates. [Protocol schema](https://agentclientprotocol.com/protocol/v1/schema)
3. **Current Kiro CLI exposes useful controls.** The installed Kiro CLI 2.27.0 accepts `--model`, `--effort`, `--agent`, engine selection, and trusted-tool policies. A local initialization-only probe (no prompt/model request) returned protocol v1 with `loadSession:true`, image prompts, and HTTP MCP support.
4. **Less maintenance for delegated work.** Changes in Kiro's request headers, model fields, or stream union are handled by Kiro CLI, not by this repository.

## Downsides of a complete switch

### 1. Provider mode is possible, but translation remains

The Oct 1 `kiro-acp-ai-provider` disproves the broad claim that ACP necessarily makes Kiro own every tool: its MCP bridge returns Kiro tool requests to the application. However, its [current README](https://github.com/NachoFLizaur/kiro-acp-ai-provider/blob/main/README.md) documents the remaining mismatches: Kiro's base context is always present; custom system prompts are XML-tag injections; temperature and similar per-turn controls are unavailable; token counts are estimated; Kiro reasoning is always streamed; fork/truncate is synthetic history replay; tool-returned images require an aborted turn and follow-up prompt.

Our direct proxy preserves the client-owned loop without starting a Kiro subprocess/MCP bridge per isolated lane, and can enforce Anthropic/OpenAI response contracts directly.

### 2. OpenAI/Anthropic compatibility still needs a second adapter

kiro-proxy exposes `/v1/messages`, `/v1/messages/count_tokens`, `/v1/chat/completions`, and `/v1/models`. ACP uses bidirectional JSON-RPC over a long-lived process. The current post-release provider targets Vercel AI SDK, not Claude Code's Anthropic endpoint or generic OpenAI SDKs. A pre-release project, [`kiro-acp-gateway`](https://github.com/robert-mcdermott/kiro-acp-gateway), demonstrates that those HTTP surfaces can be layered over ACP, but its last push was 2026-09-18—before Opus 5.5—so it is architecture evidence, not current model evidence.

An ACP cutover would therefore replace this repository's private AWS translation with subprocess/session/MCP translation plus the same OpenAI/Anthropic response translation. It moves maintenance; it does not eliminate it.

### 3. Current ACP provider still has context and concurrency races

The Oct 1 provider isolates child sessions into separate Kiro processes and has lane routing for concurrent calls. Its 3.3.0 changelog still lists two known context defects: after compaction/history rewrite, a fresh session can omit earlier history when the prompt has no assistant/tool messages; a concurrent title/auxiliary request can overwrite the saved main session affinity. Tool routing correlates only by tool name plus deep-equal arguments and falls back after two seconds to the most recently registered lane, so identical concurrent tool calls remain ambiguous. [Lane router source](https://github.com/NachoFLizaur/kiro-acp-ai-provider/blob/main/src/lane-router.ts), [session affinity source](https://github.com/NachoFLizaur/kiro-acp-ai-provider/blob/main/src/session-affinity.ts)

Kiro issue [#6640](https://github.com/kirodotdev/Kiro/issues/6640) is older evidence of multiple ACP processes hanging through a singleton child. The current provider's process isolation is mitigation, not proof that Kiro CLI 2.27.0 removed that upstream constraint.

### 4. Overload visibility is inferred, not protocol-native

Current provider 3.3.0 detects ten seconds of ACP silence, then reads Kiro's local log and derives hints such as `ModelOverloaded`. That is useful operational work, but it confirms ACP still does not reliably return structured overload/rate-limit state. The direct proxy receives HTTP/event error reasons and can emit Anthropic 529/`overloaded_error`, 429/`rate_limit_error`, `Retry-After`, and model cooldowns without scraping logs. [3.3.0 changelog](https://github.com/NachoFLizaur/kiro-acp-ai-provider/blob/main/CHANGELOG.md)

### 5. Process-tree cleanup has leaked children

Kiro issue [#6050](https://github.com/kirodotdev/Kiro/issues/6050) documents `kiro-cli acp` exiting without terminating its `kiro-cli-chat acp` child or that child's MCP servers. The issue remains open. A durable ACP integration needs process-group/job-object cleanup, orphan detection, and restart supervision; killing only the direct subprocess is insufficient.

### 6. Advertised session loading has hung

Kiro issue [#6753](https://github.com/kirodotdev/Kiro/issues/6753) reports `session/load` hanging forever for a session created by a previous ACP process even though Kiro advertises `loadSession:true`. The locally observed 2.27.0 response still advertises `loadSession:true`; persistence across process restarts was not probed because that would issue agent-session operations.

### 7. Current negotiated capabilities are narrower than the full ACP schema

The local Kiro CLI 2.27.0 initialization response was:

```json
{
  "protocolVersion": 1,
  "agentCapabilities": {
    "loadSession": true,
    "promptCapabilities": {
      "image": true,
      "audio": false,
      "embeddedContext": false
    },
    "mcpCapabilities": {
      "http": true,
      "sse": false
    },
    "sessionCapabilities": {},
    "auth": {}
  }
}
```

Therefore callers cannot assume embedded resource context, audio, SSE MCP, session close/fork/list, or other optional session capabilities. ACP capability negotiation is explicit; unsupported features cannot be inferred from the protocol's full schema.

### 8. Existing proxy controls disappear or become Kiro CLI policy

The current proxy owns multi-account selection, persisted model cooldowns, quota/usage endpoints, request IDs/debug captures, payload/image guards, per-request model aliases, both API surfaces, and client-visible error taxonomy. ACP delegates those decisions to one local Kiro CLI credential/session unless a separate supervisor reimplements them outside ACP.

### 9. Security and trust move into the ACP adapter

ACP agents may execute tools directly, or an adapter may relay tool calls through MCP/IPC to the host as `kiro-acp-ai-provider` does. Both require per-user workspace isolation, bounded process permissions, and an explicit permission policy. `--trust-all-tools` or unconditional `proceed_always` is high risk; the existing local extension instead answers each `session/request_permission` with a once-only decision (`pi-config/extensions/kiro-acp/client.ts:376-445`).

## Recommended architecture

| Need | Use |
|---|---|
| Claude Code/Codex/generic SDK compatibility with exact HTTP error, usage, stop, and reasoning policies | direct kiro-proxy backend |
| Vercel AI SDK application that accepts ACP session/history limitations and wants first-party Kiro wire handling | optional ACP provider backend |
| A whole coding task can be handed to Kiro with Kiro owning the workspace loop | Kiro ACP delegation |
| Native ACP editor/client | `kiro-cli acp` directly |

Keep both paths explicit. If ACP is added here, implement it as a backend selected per account/request—not as a replacement—and do not share one Kiro session across unrelated HTTP conversations. Reuse the current provider's useful patterns: runtime model/effort discovery, per-session model+effort application, stall watchdog, child-process isolation, and auth-probe single-flight. Do not copy its two-second ambiguous tool-lane fallback or synthetic history reset as transparent behavior.

## Migration threshold

Make ACP the default only after all of these are verified against the deployed Kiro CLI and Opus version:

1. Anthropic and OpenAI conformance tests pass through the ACP adapter, including stop reasons, tool loops, signed thinking, errors, and token counts.
2. Compaction, fork/revert, and auxiliary concurrent requests cannot lose or replace main-session context.
3. Parallel identical tool calls are correlated by an opaque session/call ID, not name+argument heuristics.
4. Process-tree shutdown leaves no children/MCP servers and bounded process-pool admission is enforced.
5. Rate-limit, overload, and stalled-turn states are structured and bounded without local-log scraping.
6. Multi-account routing and model cooldown behavior remain available.

Until then, optional ACP backend + direct default is the lower-risk cutover.

## Current-repository filter and sources

Post–Opus 5.5 repositories used for current implementation evidence:

- [`NachoFLizaur/kiro-acp-ai-provider`](https://github.com/NachoFLizaur/kiro-acp-ai-provider), pushed 2026-10-01; [3.3.0 commit](https://github.com/NachoFLizaur/kiro-acp-ai-provider/commit/1f1c8cd7ac1a0735381ddc36b08e11ba93703757), [changelog](https://github.com/NachoFLizaur/kiro-acp-ai-provider/blob/main/CHANGELOG.md), [runtime model discovery](https://github.com/NachoFLizaur/kiro-acp-ai-provider/blob/main/src/kiro-models.ts), [session affinity](https://github.com/NachoFLizaur/kiro-acp-ai-provider/blob/main/src/session-affinity.ts), [lane routing](https://github.com/NachoFLizaur/kiro-acp-ai-provider/blob/main/src/lane-router.ts).
- [`xiwan/acp-bridge`](https://github.com/xiwan/acp-bridge), pushed 2026-09-28. Its post-release commits are router/config work, not Opus 5.5 compatibility changes; its process-pool design is operational architecture evidence only.

Excluded from Opus 5.5 conclusions because their last push predates 2026-09-22: `robert-mcdermott/kiro-acp-gateway` (Sep 18), `aws-samples/sample-acp-bridge` (Jun 23), `jwadow/kiro-gateway` (May 18), `rezkycodes/kr-to-cc` (Aug 21), `sunerpy/opencode-kiro-auth` (Aug 26), and `Colin3191/kiro-proxy` (Jul 31).

Protocol/runtime sources: [official ACP repository](https://github.com/agentclientprotocol/agent-client-protocol), [v1 schema](https://agentclientprotocol.com/protocol/v1/schema), [Kiro ACP documentation](https://kiro.dev/docs/cli/acp/), local `kiro-cli 2.27.0` initialization-only capability probe, and local `pi-config/extensions/kiro-acp` implementation.