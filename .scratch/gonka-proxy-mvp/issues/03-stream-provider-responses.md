# Stream Provider responses without corrupting generations

Status: ready-for-agent

## Parent

[Gonka Proxy MVP PRD](../PRD.md)

## What to build

Extend the public chat-completions path to preserve OpenAI-compatible streaming behavior. Forward response headers and chunks promptly without buffering the complete generation. Once downstream streaming begins, never combine it with a new generation from another Provider.

User stories covered: 3, 26–27.

## Acceptance criteria

- [ ] A streaming chat-completions response reaches the client incrementally before the upstream response completes.
- [ ] Client-relevant response headers, chunk order, chunk contents, and normal stream termination are preserved.
- [ ] Successful streams are not buffered in full by the proxy.
- [ ] If an upstream stream terminates unexpectedly after transmission starts, the downstream stream closes and no second Provider is attempted for that request.
- [ ] An interrupted stream makes the Provider unavailable through the same Cooldown behavior used for other Failover Failures.
- [ ] Client cancellation closes the active upstream stream promptly.
- [ ] Black-box tests use a controllable streaming fake Provider and prove both incremental delivery and the absence of mixed generations.

## Blocked by

None - Issue 01 was resolved in commit `7e6cf76`.
