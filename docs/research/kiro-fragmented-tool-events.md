# Kiro fragmented tool events

**Research date:** 2026-10-01

## Conclusion

Kiro's `toolUseEvent` is a fragmented Smithy union member. A logical call can arrive as a first frame carrying `name` and `toolUseId`, one or more input frames, and a final frame carrying `stop`; fields are optional per frame. Dispatching by the presence of `name`, `input`, or `stop` is incorrect because identity fields may repeat and several calls may be interleaved. The parser must correlate state by `toolUseId`, preserve first-seen order, append string deltas, handle object snapshots without duplicating them, and emit only calls that reached a valid stop.

## Primary-source comparison

- [`pi-provider-kiro/src/event-parser.ts`](https://raw.githubusercontent.com/mikeyobrien/pi-provider-kiro/main/src/event-parser.ts) models `toolUseEvent` as optional `name`, `toolUseId`, `input`, and `stop`. Its parser accepts input-only and stop-only frames, serializes object inputs, and treats empty objects as no argument delta.
- [OpenCodex's Kiro event parser](https://raw.githubusercontent.com/lidge-jun/opencodex/main/src/adapters/kiro-events.ts) validates the same optional union shape. Its [stream state](https://github.com/lidge-jun/opencodex/blob/main/src/adapters/kiro/stream.ts) keys open calls by ID, preserves arrival order for parallel calls, can replace a locally generated ID when the upstream ID arrives later, and validates open calls before EOF delivery.
- [Kiro-Go's parser](https://github.com/Quorinex/Kiro-Go/blob/main/proxy/kiro.go#L690-L950) likewise tracks pending calls by `toolUseId`, preserves order, supports generated-ID rekeying, and treats an incomplete tool at EOF as retryable instead of delivering missing arguments.
- [OmniRoute #14668](https://github.com/diegosouzapw/OmniRoute/issues/14668) documents empty arguments caused by dropping object/snapshot-shaped tool inputs. It supports accepting arbitrary input shapes and avoiding duplicate snapshots rather than assuming every input is a string delta.
- [kiro.rs #22](https://github.com/hank9999/kiro.rs/issues/22) captures the normal Anthropic-facing shape: a tool starts with `input:{}`, followed by multiple `input_json_delta.partial_json` fragments before tool stop. Empty start input is not proof of an empty completed call.
- [OmniRoute #2446](https://github.com/diegosouzapw/OmniRoute/issues/2446) documents follow-up failures when translated tool IDs and result references diverge, reinforcing stable ID preservation across the complete call lifecycle.

## Implemented invariants

`internal/awsstream` now:

1. Routes binary frames by `:event-type`, then correlates `toolUseEvent` fragments by `toolUseId`.
2. Supports repeated identity fields, input-only and stop-only continuations, interleaved calls, and late upstream IDs for a uniquely identifiable generated call.
3. Preserves first-seen tool order independently from completion order.
4. Appends string deltas; accepts object/array snapshots and deduplicates repeated snapshots.
5. Rejects empty events, invalid IDs/names, same-ID name changes, ambiguous ID-less continuations, unsupported input shapes, and post-completion non-terminal frames.
6. Makes duplicate terminal frames idempotent for the same completed call.
7. Rejects open or invalid tool calls at EOF; it never converts an incomplete binary call to `{}`.

## Regression evidence

The original defect is locked down by a frame-level test where three CRC-valid `toolUseEvent` frames repeat the same `name`/`toolUseId` while splitting `{"command":"pwd"}` across input deltas. Before the fix, the parser finalized fragments separately and returned `"pwd"`; after the fix it returns one call with the exact argument object. Additional tests cover byte-at-a-time transport reads, interleaved calls, generated-ID rekeying, duplicate object snapshots, duplicate stop frames, malformed identities, name mutation, and open-call EOF.
