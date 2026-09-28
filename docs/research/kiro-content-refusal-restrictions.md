# Kiro content-refusal restrictions and retry storms

**Research date:** 2026-09-28
**Scope:** Kiro/AWS first-party documentation and support material, Kiro's public issue tracker, and public Kiro proxy/client implementations.

## Executive summary

The exact upstream text reported by this repository—`Your access to this model has been temporarily restricted due to excessive content refusals`—is also reported for Opus 5.5 in [Kiro issue #11635](https://github.com/kirodotdev/Kiro/issues/11635). The issue remains pending maintainer response. Kiro's public documentation and changelog do not define the refusal counter, threshold, duration, response status, or wire-format contract. **Do not infer that a particular HTTP status, retry-after value, or account-wide scope is guaranteed.**

The observable behavior relevant to this proxy is nevertheless clear from the code and public reports:

* Kiro can terminate or fail a request before producing a recognized event, including an HTTP 200 response with no bytes. The repository already models this as `UpstreamStreamError` and has an explicit zero-byte predicate.
* A request that has not produced client-visible output is safe to replay. Once output has been forwarded, replaying can concatenate two answers or duplicate tool calls; public proxy implementations therefore treat mid-stream failures as terminal.
* A restriction/refusal signal is not a transient transport failure. Retrying the same prompt/model/account immediately can amplify the condition. The proxy should classify the exact message as **non-retryable for that model/account**, open a cooldown, return a truthful error, and permit a later half-open probe.
* HTTP 200 with zero bytes is an important **restriction/capacity suspicion signal**, but it is not proof of a content refusal. Keep it separate from an explicitly matched refusal message and use conservative protection rather than claiming the cause.

## What first-party sources establish

### Exact message and refusal policy

Kiro's public issue [#11635](https://github.com/kirodotdev/Kiro/issues/11635) contains the exact message and identifies Opus 5.5 as the affected model. This supports model-scoped protection as the conservative proxy response, but it is a user report—not a protocol specification—and has no maintainer resolution yet.

No first-party Kiro documentation defines the message's status code, trigger threshold, scope, or duration. Kiro's public [Models documentation](https://kiro.dev/docs/models/) says Kiro provides models from Anthropic, OpenAI, and other providers, but does not document content-refusal accounting or retry semantics. Kiro's [IDE troubleshooting guide](https://kiro.dev/docs/ide/troubleshooting/) covers common client/network failures (including TLS inspection), not refusal restrictions. This is a documented gap, not evidence that the message is undocumented internally.

Kiro does document administrative model availability: [model governance](https://kiro.dev/docs/enterprise/governance/model/) says administrators can manage an approved model list, and that a newly offered model is unavailable to clients until added to that list. This is a distinct, deterministic entitlement failure—not evidence about content-refusal restrictions—and should not be handled as a generic retry storm.

The public [Kiro issue tracker](https://github.com/kirodotdev/Kiro/issues/8001) also records a user seeing persistent 429 responses after an account was unblocked from a security precaution. The maintainer response directs account-state cases to Kiro support and requests the account/user ID, plan, quota, request IDs, start time, and affected surfaces. This confirms that account-level restriction state may survive an apparent unblock, but it does **not** establish a retry contract for the content-refusal message.

### AWS streaming semantics that are relevant, but not Kiro-specific

AWS's [Bedrock `ConverseStream` API reference](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ConverseStream.html) documents streaming model responses and model-dependent behavior. It does not document this Kiro message or authorize replaying a partially delivered stream. The AWS reference is useful background only; Kiro's private runtime protocol may differ.

## Public proxy/client evidence

These repositories were searched because they implement Kiro-compatible gateways or clients. The links below point directly to source, issue, or repository material; reports are observations by maintainers/users, not Kiro guarantees.

### `jwadow/kiro-gateway`

* Repository: [jwadow/kiro-gateway](https://github.com/jwadow/kiro-gateway)
* The gateway's [streaming core](https://raw.githubusercontent.com/jwadow/kiro-gateway/main/kiro/streaming_core.py) treats an empty async response (`StopAsyncIteration`) as a normal end of stream. Its first-token timeout is handled separately. This is a useful caution: treating empty 200 as success hides a restriction and can make a client believe it received a valid short answer.
* [Issue #129](https://github.com/jwadow/kiro-gateway/issues/129) reports `RemoteProtocolError: peer closed connection without sending complete message body` during chunked streaming. The issue says initial-request retry does not cover mid-stream failures and that retry is unsafe after partial content. A commenter speculates about credit-related throttling; the maintainer explicitly says that was not confirmed. Treat the credit correlation as unverified, not as policy.

### `Quorinex/Kiro-Go`

* Repository: [Quorinex/Kiro-Go](https://github.com/Quorinex/Kiro-Go)
* [Issue #142](https://github.com/Quorinex/Kiro-Go/issues/142) identifies a stream parser that returned immediately on a stream failure even when no content had reached the client. The proposed fix tracks an `emitted` flag: retry before any client-visible output, but return terminally after output. The report also notes that rapid same-second attempts tended to fail together and that a short backoff improved recovery. These are implementation observations, not an upstream SLA.

### `lidge-jun/opencodex`

* [Issue #519](https://github.com/lidge-jun/opencodex/issues/519) describes heartbeat-only streams followed by socket close with zero output and argues that replay is safe only because no output was relayed. It is useful corroboration for the output-commit boundary; it does not establish that heartbeat-only close means content refusal.

### Other searched public projects

* [`Colin3191/kiro-proxy`](https://github.com/Colin3191/kiro-proxy)
* [`Lypt1x/gateway`](https://github.com/Lypt1x/gateway)
* [`coderhisham/kiro-gateway-next`](https://github.com/coderhisham/kiro-gateway-next)
* [`diegosouzapw/OmniRoute`](https://github.com/diegosouzapw/OmniRoute)
* [`ducphamzyr/Kiro-GoPlus`](https://github.com/ducphamzyr/Kiro-GoPlus)

Search results and issue reports in these projects discuss Kiro translation, empty responses, rate limits, and stream failures, but I found no maintained implementation that documents a reliable, Kiro-specific refusal threshold or cooldown duration. In particular, a generic `429` handler must not be assumed to distinguish ordinary rate limiting, quota exhaustion, security holds, and content-refusal restrictions.

## Repository behavior and implications

The current Go proxy already has the right primitives:

* [`internal/streaming/core.go`](../../internal/streaming/core.go) defines `UpstreamStreamError` for HTTP 200 streams ending with an embedded exception or no recognized event, and `IsEmptyResponse` requires `BeforeFirstEvent && BytesRead == 0`.
* `StreamWithFirstTokenRetry` retries first-token timeout and non-empty before-first-event failures, but deliberately does **not** retry the zero-byte 200 case. That is the correct anti-amplification direction for a suspected restriction; do not turn zero-byte protection into an unconditional retry loop.
* [`internal/api/openai/routes.go`](../../internal/api/openai/routes.go) and the corresponding Anthropic route preserve the pre-header boundary: errors before the first client-visible event can become a real HTTP error; errors after headers are terminal stream errors.
* [`internal/account/manager.go`](../../internal/account/manager.go) opens model-specific protection after an empty response and emits `Retry-After`. The existing comments correctly describe this as protection after a **possible** temporary restriction, not proof of one.
* [`internal/auth/errors.go`](../../internal/auth/errors.go) classifies 402/403/429 and `INVALID_MODEL_ID` as recoverable account failures, while treating malformed requests, context overflow, validation errors, and 5xx as fatal. An explicit content-refusal restriction should not be silently folded into the generic 429/account-failover path without checking the response body and stream metadata.

## Recommended signal detection

Implement detection at the upstream normalization boundary, before generic status classification:

1. **Exact refusal text:** normalize Unicode/whitespace only for matching; compare case-insensitively against `your access to this model has been temporarily restricted due to excessive content refusals`. Preserve the original message for logs/client output. Also retain a small, reviewed family of exact variants only when observed from Kiro; do not use a broad substring such as `refusal` that could match ordinary assistant text.
2. **Structured fields:** inspect JSON or embedded event-stream exception fields named `message`, `reason`, `error`, and any upstream error type/code. Record status, reason, request ID, model ID, and whether the signal arrived before any recognized/client-visible event. Never log prompt contents or credentials.
3. **HTTP status:** inspect non-200 responses, especially 403 and 429, but classify by structured reason/message first. A bare 429 is ordinary rate limiting or another account state until proven otherwise; a bare 403 may be credential, entitlement, security, or restriction state. Do not label either as content refusal without the message.
4. **HTTP 200 stream:** distinguish (a) zero bytes, (b) bytes with no recognized events, (c) embedded exception text, and (d) recognized events followed by EOF/read error. Zero-byte 200 is a restriction/capacity suspicion signal and should use model protection; it is not confirmation of the exact refusal.
5. **Commit state:** track whether any client-visible content, thinking output, tool call, or equivalent event was forwarded. Upstream bytes alone are not sufficient: parser headers/heartbeats and unrecognized bytes do not make replay unsafe.

## Retry and non-retry policy

| Signal | Replay upstream request? | Account failover? | Action |
|---|---:|---:|---|
| Exact content-refusal restriction message, before output | **No immediate retry** | **No automatic account rotation by default** | Open model/account cooldown; return a distinct 423/429-style proxy error with `Retry-After`; preserve request ID. The message may be account-scoped, and spraying identical content across accounts can amplify abuse detection. |
| Exact message after output | **Never** | **No** | Emit terminal stream error; output is already committed. Do not synthesize a normal stop reason. |
| HTTP 200, zero bytes, no event | **No immediate retry** | **No automatic storm** | Open model-specific protection. Return a distinct temporary-restriction/empty-response error. A later half-open probe may retry once. |
| HTTP 200, non-empty but no recognized event / embedded capacity error, no output | **At most bounded retry** | **Only if reason explicitly indicates account/model entitlement and policy allows** | Retry once or through the configured first-event budget with jittered backoff; stop on a matched restriction message. |
| First-token timeout, no output | **Bounded retry** | Optional | Existing first-token retry is appropriate; add exponential backoff + jitter and a strict total attempt/time budget. |
| Transport/EOF/read failure before output | **Bounded retry** | Optional | Replay only when no client-visible output was sent; use backoff and a per-request retry budget. |
| Transport/EOF/read failure after output | **No** | **No** | Terminal stream error; do not concatenate a second answer. |
| 429 without refusal text | **Bounded, `Retry-After` aware** | Maybe, if account policy says rate limit is account-specific | Honor upstream `Retry-After`; cap attempts and cooldown. Avoid treating repeated 429 as quota exhausted without evidence. |
| 403/402/`INVALID_MODEL_ID` with known structured reason | Existing auth/account policy | Existing policy | Keep current reason-based classification. `INVALID_MODEL_ID` can fail over only when another account may be entitled; credential/security holds need support, not retries. |
| 4xx malformed request/context/validation | **No** | **No** | Return to client; another account cannot fix the payload. |
| 5xx | Usually no immediate account rotation | Usually no | Bound retries only at the transport/request layer with jitter; do not multiply load across accounts during a regional outage. |

The key distinction is **replay safety**, not whether an error “looks temporary.” A body can be replayed only if the client has not seen output. A restriction can be temporary while still being non-retryable for the current cooldown window.

## Cooldown, concurrency, and circuit-breaker recommendations

1. **Scope protection conservatively.** Key the restriction breaker by `(account, model)`; if the upstream signal includes no account/model scope, protect the selected account+model rather than every account and every model. Keep exact-message restriction state separate from generic transport failure counters.
2. **Use a cooldown, not a tight retry loop.** Honor a trustworthy upstream `Retry-After`; otherwise use the configured model-restriction cooldown (the repository currently defaults to one hour when unset). Add small random jitter to release times so concurrent callers do not stampede at the boundary.
3. **Single-flight half-open probes.** When cooldown expires, allow one probe for that key. Other callers should receive the remaining `Retry-After` or use a different explicitly available model, not all issue simultaneous probes. Success closes the breaker and clears consecutive restriction failures; another restriction reopens it with bounded exponential backoff.
4. **Do not use client concurrency as a workaround.** Limit in-flight requests per account/model while a restriction or repeated zero-byte signal is active. A global semaphore prevents one hot model from generating a retry storm, but do not block unrelated healthy models unnecessarily.
5. **Avoid automatic prompt mutation.** The proxy cannot safely rewrite potentially sensitive user content to evade a safety control. Surface the restriction and let the caller change the request or wait. Do not rotate accounts to bypass a policy restriction.
6. **Cap all retry dimensions.** Enforce a per-request maximum attempt count and deadline, plus per-key breaker state. Client retries, first-event retries, account failover, and model fallback must share a budget or coordinate; independent loops multiply traffic.
7. **Observe without collecting sensitive content.** Count signals by hashed/request-scoped identifiers and model/account labels; record status, reason class, bytes/events before failure, cooldown transitions, and upstream request ID. Do not persist prompts, auth headers, raw stream bodies, or account tokens.

## Uncertainty and operational response

The exact message may be generated by a safety or entitlement layer whose wire behavior is not publicly documented. Therefore the proxy should report: “Kiro returned a temporary model restriction; automatic retry is suppressed until `<time>`,” rather than asserting that the user exceeded a known threshold or that the restriction is definitely account-wide. Include the upstream request ID when available and direct account-state cases to Kiro support. Kiro issue #11635 currently has no maintainer resolution; issue #8001 asks users to provide account/user ID, plan, quota, request IDs, start time, and affected surfaces to support.

A zero-byte 200 response can result from an upstream restriction, capacity/routing behavior, proxy/network termination, or an undocumented runtime failure. The repository's existing model protection is justified as a defensive inference, not a confirmed Kiro protocol fact. Keep logs and metrics able to distinguish these cases so a future Kiro response field or support clarification can narrow the policy.

## Sources

* Kiro, [Models](https://kiro.dev/docs/models/)
* Kiro, [IDE troubleshooting](https://kiro.dev/docs/ide/troubleshooting/)
* Kiro, [Enterprise model governance](https://kiro.dev/docs/enterprise/governance/model/)
* Kiro public issue, [Exact Opus 5.5 content-refusal restriction message (#11635)](https://github.com/kirodotdev/Kiro/issues/11635)
* Kiro public issue, [Persistent 429 rate limit after account unban (#8001)](https://github.com/kirodotdev/Kiro/issues/8001)
* AWS, [Bedrock ConverseStream API reference](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ConverseStream.html)
* jwadow, [kiro-gateway](https://github.com/jwadow/kiro-gateway), [streaming core](https://raw.githubusercontent.com/jwadow/kiro-gateway/main/kiro/streaming_core.py), [issue #129](https://github.com/jwadow/kiro-gateway/issues/129)
* Quorinex, [Kiro-Go issue #142](https://github.com/Quorinex/Kiro-Go/issues/142)
* lidge-jun, [opencodex issue #519](https://github.com/lidge-jun/opencodex/issues/519)
* Colin3191, [kiro-proxy](https://github.com/Colin3191/kiro-proxy)
* Lypt1x, [gateway](https://github.com/Lypt1x/gateway)
* coderhisham, [kiro-gateway-next](https://github.com/coderhisham/kiro-gateway-next)
* diegosouzapw, [OmniRoute](https://github.com/diegosouzapw/OmniRoute)
* ducphamzyr, [Kiro-GoPlus](https://github.com/ducphamzyr/Kiro-GoPlus)
