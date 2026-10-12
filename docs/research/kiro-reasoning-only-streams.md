# Kiro reasoning-only and empty streams

**Research date:** 2026-10-01  
**Scope:** Kiro-compatible streaming implementations and first-party AWS/Kiro-adjacent evidence. This note separates observed repository behavior from claims made by maintained adapters; Kiro does not publish a complete public runtime event-stream specification.

## Executive summary

The reported HTTP 200 streams are incomplete turns, not successful answers: `contextUsageEvent` is bookkeeping, and `reasoningContentEvent` is reasoning, while a normal text/tool turn requires a final `assistantResponseEvent` or `toolUseEvent` (and usually terminal metadata). Parsing `reasoningContentEvent` would recover or expose reasoning, but **would not by itself create the missing assistant answer or fix the user-visible failure**. A proxy MUST NOT promote private reasoning to a final answer or synthesize a normal stop merely because reasoning arrived.

The safest recovery rule supported by independent maintained implementations is the output-commit boundary: replay only while no client-visible text, reasoning, or tool output has been forwarded. Metadata-only EOF is safe to replay. Reasoning-only EOF is replayable only if reasoning was held back and nothing was forwarded; after forwarding reasoning, replay is unsafe because it can duplicate visible content/tool side effects. If a stream has already committed output, surface a terminal incomplete-stream error rather than concatenate a second answer.

The model-specific Claude-vs-GPT explanation is **not proven** by the available public sources. The deployment observation (Claude failures and GPT successes in the same session) establishes correlation only. OpenCodex's maintained parser documents a different reasoning payload shape for the GPT-5.6 `sol`/`terra`/`luna` family, but that does not establish why Claude Opus 5.5 produces reasoning-only EOFs.

## Wire event union and payloads

There is no public Kiro document that specifies the complete private union. The strongest directly inspectable contract is the maintained OpenCodex discriminated parser, [`src/adapters/kiro-events.ts`](https://raw.githubusercontent.com/lidge-jun/opencodex/main/src/adapters/kiro-events.ts): it dispatches by the Smithy `:event-type` header and recognizes `assistantResponseEvent`, `reasoningContentEvent`, `toolUseEvent`, `messageMetadataEvent`, `initial-response`, `metadataEvent`, `meteringEvent`, `contextUsageEvent`, `invalidStateEvent`, and `error`.

* `assistantResponseEvent` → `{content?: string, modelId?: string}` and becomes ordinary assistant text.
* `reasoningContentEvent` → optional plaintext `text`, or `signature` / `redactedContent` fields. OpenCodex comments that captures of GPT-5.6-family `sol`/`terra`/`luna` use an encrypted `.KTR~~…` `signature`; it accepts the fields by wire shape, not model ID. This is reasoning/blob transport, not a final answer.
* `toolUseEvent` → optional `name`, `toolUseId`, `input`, and boolean `stop`; it represents a tool turn, not ordinary text.
* `contextUsageEvent` → required finite `contextUsagePercentage`. OpenCodex explicitly says this is the authoritative context-pressure event in captured kiro-cli 2.14.1/2.16.0 traffic; `metadataEvent.contextUsagePercentage` is only a fallback.
* `metadataEvent` → optional `stopReason`, `tokenUsage`, and fallback context percentage. `stopReason` is Kiro's terminal verdict, but a stop reason does not necessarily prove ordinary text is a final answer: OpenCodex validates completion separately for tool-enabled turns.
* `meteringEvent` → `unit`, numeric `usage` (or `amount`), and optional `unitPlural`.
* `messageMetadataEvent` / `initial-response` → conversation/utterance identifier.
* `invalidStateEvent` → optional `message`.
* `error` (or a Smithy `:message-type` of `error`/`exception` in other implementations) → reason/type plus message. Kiro-Go's parser reads the `:message-type` header and returns the embedded `message` as an upstream stream error; see [`proxy/kiro.go`](https://github.com/Quorinex/Kiro-Go/blob/main/proxy/kiro.go#L560-L690).

The official AWS Toolkit change [PR #4448](https://github.com/aws/aws-toolkit-vscode/pull/4448) confirms that CodeWhispererStreaming's generated `ChatResponseStream` union gained an `invalidStateEvent` member with a diagnostic message. It is useful first-party evidence for the service's generated event-union approach, but it does not publish the newer Kiro reasoning/metadata shapes.

## Three maintained implementations and recovery semantics

### Kiro-Go

[Kiro-Go's parser](https://github.com/Quorinex/Kiro-Go/blob/main/proxy/kiro.go#L560-L690) decodes AWS EventStream frames, validates CRCs, switches on `:event-type`, and handles assistant text, reasoning text, tools, metering, context usage, and metadata stop reason. `assistantResponseEvent` and non-empty `reasoningContentEvent` set internal `sawOutput`; context/metering/metadata do not. Clean EOF with no `sawOutput` returns `errEmptyKiroStream` (`upstream stream ended before any output`). A separate `emitted` flag is set only when a configured client callback was actually invoked.

The call path retries an empty/pre-output stream with 700 ms backoff, up to two attempts per endpoint across its endpoint list, but returns terminally once output has been delivered. Thus metadata-only EOF is recognized as empty and replayable. Reasoning-only EOF is *not* a special incomplete-final-answer class there: reasoning counts as model output, so it can complete as a partial successful parse if the reasoning callback is wired. This is a material caution against assuming that “reasoning arrived” means “the answer completed.” Issue [#142](https://github.com/Quorinex/Kiro-Go/issues/142) documents HTTP-successful premature EOFs, failures both before and after partial output, 5/5 recovery by client retry, and why synthesizing normal `message_delta`/`stop_reason` would hide truncation.

### OpenCodex

OpenCodex's [event parser](https://raw.githubusercontent.com/lidge-jun/opencodex/main/src/adapters/kiro-events.ts) is frame-aware and validates known payload shapes. Its [stream state machine](https://raw.githubusercontent.com/lidge-jun/opencodex/main/src/adapters/kiro/stream.ts#L780-L1130) tracks `sawText`, `sawReasoning`, real tools, stop reason, deferred output, and prior emitted output independently. At clean EOF with no text/reasoning/tool it emits a named retryable `empty_kiro_stream`. When completion validation is enabled, reasoning without a validated final answer is named `reasoning_only_kiro_fallback`; no reasoning is `empty_kiro_fallback`. These are retryable only when the same logical turn has not already emitted output.

Its catch path uses [`isRetryableKiroStreamCatchError`](https://raw.githubusercontent.com/lidge-jun/opencodex/main/src/adapters/kiro/stream.ts#L205-L230): transport-like closes/decoder truncation are replayable only when no output/state was emitted; malformed `invalid Kiro …` payloads and all post-output failures are terminal. Issue [#519](https://github.com/lidge-jun/opencodex/issues/519) records a 200 response with heartbeats and zero output ending in a socket close; request size was not the trigger. Merged [PR #520](https://github.com/lidge-jun/opencodex/pull/520) changed the hard-coded terminal 502 to a retryable result only for pre-output transport failures. This is the clearest external match for the safety boundary.

### pi-provider-kiro

[pi-provider-kiro's stream implementation](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/stream.ts) uses AWS Smithy's maintained `UniversalEventStreamMarshaller`, rather than a hand-rolled frame decoder. Its README and stream retry loop describe local recovery for first-token/stalled streams and empty responses, while generic 429/5xx handling is delegated to the host. The loop distinguishes degenerate attempts with no text/tool calls, resets discarded attempt output before replay, and reports a terminal diagnostic after the bounded empty-response budget is exhausted; see the `emptyAttempts`, `receivedContextUsage`, and `degenerate` handling around [the retry loop](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/stream.ts#L695-L740) and [the EOF/empty-response branch](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/stream.ts#L1680-L1785). Its local retry helper [retry.ts](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/retry.ts) classifies Kiro HTTP body reason codes, not the full event union.

This implementation is evidence that empty-stream recovery is a maintained adapter concern, but its public code does not establish that every reasoning-only EOF is safe to return as a completed turn. The safer common denominator remains “no forwarded output before replay”; reasoning must not be silently converted into final text.

### Kiro Gateway (contrast)

[`jwadow/kiro-gateway`'s parser](https://raw.githubusercontent.com/jwadow/kiro-gateway/main/kiro/parsers.py#L180-L290) is materially different: it decodes chunks as UTF-8 and shape-scans embedded JSON prefixes (`{"content":`, `{"usage":`, `{"contextUsagePercentage":`, tool fragments), without AWS frame/CRC decoding or a `:event-type` union. It therefore cannot reliably distinguish `reasoningContentEvent` from other event types. Its documented recovery is principally synthetic truncation follow-up messages, not transparent replay of the empty/reasoning-only request. It is useful as evidence of an alternative adapter, not as authoritative wire semantics.

## Implemented repository behavior

The proxy now decodes and CRC-validates the AWS EventStream envelope in [`internal/awsstream/eventstream.go`](../../internal/awsstream/eventstream.go), routes typed frames by `:message-type` / `:event-type`, and preserves reasoning, context, usage, metadata stop reasons/details, exceptions and unknown union members. The old JSON-shape scanner remains only as a legacy/plain-JSON compatibility path.

[`internal/streaming/core.go`](../../internal/streaming/core.go) holds reasoning and metadata until an `assistantResponseEvent` or completed `toolUseEvent` makes the turn usable. If EOF arrives first, none of that attempt reaches the client: the proxy classifies `reasoning_only`, `metadata_only`, `empty_response`, truncation or transient upstream failures and performs bounded retries with 1s/2s backoff. Recovery payloads disable both catalog-native and emulated reasoning, preventing another reasoning-only turn and ensuring retries do not contain the injected XML protocol. Structured refusal/content-filter stops and terminal validation exceptions are not retried.

Model discovery now retains `additionalModelRequestFieldsSchema`. When Kiro advertises `output_config.effort` or `reasoning.effort`, the first attempt uses that native field; Claude `output_config` also carries adaptive thinking and requests summarized display only when the schema permits it. XML reasoning tags remain a compatibility fallback only for catalog models without native reasoning fields.

Binary `toolUseEvent` assembly is ID-indexed, not field-presence-indexed. Repeated `name`/`toolUseId` frames continue the same call, multiple calls may be interleaved, empty `input:{}` start markers are ignored, object input snapshots are deduplicated, and only valid stopped calls are exposed. An open call at EOF is a retryable incomplete stream rather than a successful `{}` tool invocation. Detailed source comparison and regression evidence are in [`kiro-fragmented-tool-events.md`](kiro-fragmented-tool-events.md).

Successful responses preserve Kiro's `metadataEvent.stopReason` and `stopDetails`. The OpenAI surface maps `MAX_TOKENS` to `finish_reason=length` and tool use to `tool_calls`; the Anthropic surface maps max tokens, stop sequence, refusal and tool use to their native stop reasons. After client-visible output, a later stream error emits a terminal error and never appends a normal success stop or `[DONE]` marker.

## Model-specific evidence and limits

* Proven: the deployment observed Claude Opus 5.5 reasoning/context-only HTTP 200 EOFs while GPT-5.6-sol succeeded, and small direct Claude requests can succeed. This is an observation, not a protocol explanation.
* Proven from OpenCodex source: GPT-5.6-family `sol`/`terra`/`luna` reasoning may arrive as an encrypted `signature` blob rather than plaintext `text`; the parser keys off fields, not model ID. This does not show that GPT is more reliable or that Claude is intentionally reasoning-only.
* Not proven: context size, model safety policy, account state, model routing, or Claude-vs-GPT backend behavior as the cause. The external issues report both smaller failures and larger successes, but those are user reports and do not establish Kiro's internal policy.

## Claude-specific evidence and this incident

Anthropic's public issue tracker contains two independently reported Claude thinking-only shapes:

* [Claude Code #81016](https://github.com/anthropics/claude-code/issues/81016) reports Opus returning `stop_reason=end_turn` with a thinking block but no text or tool use. It reproduces at low context, is more frequent at high reasoning effort, and consumed 246–18,512 output tokens in observed turns. This is the closest public structural match to the deployment: Kiro delivered `reasoningContentEvent` frames and then ended without `assistantResponseEvent` or `toolUseEvent`.
* [Claude Code #68882](https://github.com/anthropics/claude-code/issues/68882) reports a different shape: thinking consumes the complete output budget and the response ends with `stop_reason=max_tokens`. This remains possible in general, but the deployment did not receive a `metadataEvent` carrying `stopReason`, so it cannot be established for this incident. The proxy also does not forward the client's `max_tokens` as a Kiro model-output limit; it uses the value only to derive its injected fake-reasoning budget.

The failing conversation concerned a security-sensitive credential workflow, while small neutral requests to the same Claude models succeeded and GPT-5.6-sol completed the workflow. This makes a Claude/Kiro safety intervention plausible, but **not proven**: Kiro sent neither a structured exception nor `metadataEvent.stopReason`. Anthropic documents that classifier intervention is represented by `stop_reason: refusal` plus `stop_details`, and requires resetting or rephrasing the triggering context rather than blindly continuing it ([Handle streaming refusals](https://platform.claude.com/docs/en/test-and-evaluate/strengthen-guardrails/handle-streaming-refusals)). The absence of those fields means the proxy must classify this as `reasoning_only`/`incomplete`, not claim `refusal`.

The deployed evidence also rules out a general Claude outage and a simple payload threshold: during the same period, Claude Opus 5.5 succeeded on other requests around 528–556 KB / 185–191 messages, while the affected conversation failed around 44 KB / 4 messages and later around 121 KB / 38 messages. GPT-5.6-sol succeeded between those failures. The discriminator is conversation/model behavior, not request size alone.

## Decision: safest proxy behavior

1. Decode the actual AWS EventStream envelope and retain the event-type/payload union, or at minimum extend the current normalization layer with explicit counters for assistant text, reasoning, tools, metadata, context, and exception frames. Do not shape-scan reasoning into ordinary content.
2. Hold metadata and (if the API contract permits exposing it) reasoning until the proxy has decided whether a final assistant text/tool event exists. Treat `contextUsageEvent` and `metadataEvent` as bookkeeping, never as successful output.
3. On clean EOF with only context/metadata: classify as incomplete and retry with a bounded budget only if no client-visible output has been sent. On reasoning-only EOF: classify separately as incomplete/reasoning-only; retry only while reasoning has not been forwarded. Do not synthesize a normal final answer or stop reason.
4. After any client-visible text, reasoning, or tool event, do not replay. Return a terminal stream error/incomplete signal and preserve the missing-stop/truncation indication. This avoids duplicate text and repeated tool side effects.
5. Preserve and classify `metadataEvent.stopReason`/`stopDetails`: `MAX_TOKENS` is retryable only with an adjusted budget; `CONTENT_FILTERED`, `GUARDRAIL_INTERVENED`, or a refusal stop must be surfaced and must not be replayed unchanged. If no terminal metadata exists, report `reasoning_only` or `empty`, not a fabricated refusal reason.
6. Keep exception frames distinct from clean EOF. Preserve structured `reason`, `message`, and stop details for diagnostics, but do not broaden retries for malformed payloads or policy/content restrictions.

A raw capture would materially reduce uncertainty. It should record only frame headers/event types, payload field names and lengths (redacting prompt text, reasoning text, signatures, credentials), byte/chunk boundaries, HTTP status/headers/request ID, termination (`EOF`, read error, exception), and whether any output was forwarded. It would establish whether the reported frames are genuine Smithy `reasoningContentEvent`/`contextUsageEvent` frames, whether a `metadataEvent.stopReason` or exception was omitted, and whether the stream ended cleanly or was truncated in the envelope. It would not by itself prove why Kiro selected the model-specific behavior.

## Sources

* AWS Toolkit VS Code, [PR #4448: update generated CodeWhispererStreaming and handle invalidStateEvent](https://github.com/aws/aws-toolkit-vscode/pull/4448)
* OpenCodex, [Kiro event union and payload parser](https://raw.githubusercontent.com/lidge-jun/opencodex/main/src/adapters/kiro-events.ts), [stream state/recovery](https://raw.githubusercontent.com/lidge-jun/opencodex/main/src/adapters/kiro/stream.ts)
* Quorinex, [Kiro-Go EventStream parser/retry](https://github.com/Quorinex/Kiro-Go/blob/main/proxy/kiro.go), [issue #142](https://github.com/Quorinex/Kiro-Go/issues/142)
* mikeyobrien, [pi-provider-kiro stream](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/stream.ts), [retry helpers](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/retry.ts)
* jwadow, [kiro-gateway parser](https://raw.githubusercontent.com/jwadow/kiro-gateway/main/kiro/parsers.py)
* lidge-jun, [OpenCodex issue #519](https://github.com/lidge-jun/opencodex/issues/519), [PR #520](https://github.com/lidge-jun/opencodex/pull/520)
* Anthropic, [Handle streaming refusals](https://platform.claude.com/docs/en/test-and-evaluate/strengthen-guardrails/handle-streaming-refusals)
* Anthropic Claude Code, [Opus thinking-only `end_turn` reports (#81016)](https://github.com/anthropics/claude-code/issues/81016), [thinking-only `max_tokens` report (#68882)](https://github.com/anthropics/claude-code/issues/68882)
* Local implementation: [`internal/awsstream/parser.go`](../../internal/awsstream/parser.go), [`internal/streaming/core.go`](../../internal/streaming/core.go), [`internal/api/anthropic/streaming.go`](../../internal/api/anthropic/streaming.go)
