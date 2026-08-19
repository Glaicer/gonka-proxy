# Stream Provider responses without corrupting generations

Status: resolved

## Parent

[Gonka Proxy MVP PRD](../PRD.md)

## What to build

Extend the public chat-completions path to preserve OpenAI-compatible streaming behavior. Forward response headers and chunks promptly without buffering the complete generation. Once downstream streaming begins, never combine it with a new generation from another Provider.

User stories covered: 3, 26–27.

## Acceptance criteria

- [x] A streaming chat-completions response reaches the client incrementally before the upstream response completes.
- [x] Client-relevant response headers, chunk order, chunk contents, and normal stream termination are preserved.
- [x] Successful streams are not buffered in full by the proxy.
- [x] If an upstream stream terminates unexpectedly after transmission starts, the downstream stream closes and no second Provider is attempted for that request.
- [x] An interrupted stream makes the Provider unavailable through the same Cooldown behavior used for other Failover Failures.
- [x] Client cancellation closes the active upstream stream promptly.
- [x] Black-box tests use a controllable streaming fake Provider and prove both incremental delivery and the absence of mixed generations.

## Blocked by

None - Issue 01 was resolved in commit `7e6cf76`.

## Comments

### 2026-08-19

Resolved with incremental response flushing, transparent stream forwarding, interruption cooldowns without mixed-provider generations, cancellation propagation, and black-box coverage. `go test ./...` and `go vet ./...` pass.
