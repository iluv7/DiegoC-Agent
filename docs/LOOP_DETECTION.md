# Tool-call loop detection

The CLI enables `agent.LoopDetectionMiddleware` by default. Foreground and
background forks inherit the factory and receive independent detector instances.
The middleware wraps main reasoning model calls; summaries and goal evaluation
calls do not consume the loop budget.

A fingerprint includes each tool's name and full normalized arguments, excludes
call IDs, and ignores the order of calls in a batch. Multiplicity is preserved.
This detects identical batches, including nonconsecutive repeats, within the last
20 tool-bearing model responses. Different paging offsets or write payloads remain
distinct. It does not detect arbitrary cycles involving changed arguments or
repeated individual calls inside different batches.

The third occurrence queues one warning for the next model request, after all
existing tool results. Warnings are transient user messages in the outgoing
request, not persisted history. Failed model calls retain the queued warning for
retry. An evicted fingerprint can warn again if it reappears.

The fifth occurrence returns `*agent.LoopDetectedError` before recording the
assistant response or executing any tools in that batch. Existing tool/result
pairs remain intact. The normal error path pauses an active goal and prevents
hidden continuations from bypassing the stop. A later user reply starts a fresh
budget. Detection history also resets on cancellation or failure.

Library users opt in with `SetMiddlewareFactories`; this replaces the existing
chain, so register all desired factories together. Use
`NewLoopDetectionMiddleware(LoopDetectionConfig{WarnThreshold: 3, HardLimit: 5,
WindowSize: 20})` inside a factory. Zero fields select these defaults; invalid
thresholds return an error. Instances must not be shared across agents.

Reference: [deer-flow loop detection](https://github.com/bytedance/deer-flow/blob/main/backend/packages/harness/deerflow/agents/middlewares/loop_detection_middleware.py).
This implementation adopts its stable call-set fingerprints, sliding window,
deferred warning injection and run isolation. It uses this project's
`OnModelCall` return path as the post-model hook. Unlike deer-flow's response
stripping, a typed error reports the interruption explicitly and works with this
project's goal pause behavior. Per-tool frequency detection and deer-flow-specific
argument heuristics are outside this change.
