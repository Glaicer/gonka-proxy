# Recover indefinitely until success or client cancellation

Status: resolved

## Parent

[Gonka Proxy MVP PRD](../PRD.md)

## What to build

When no Provider remains available, keep the request alive through a Recovery Wait, clear every Provider's Cooldown, and begin a new Routing Pass. Repeat this cycle without an attempt limit until a Provider succeeds or the downstream request is cancelled.

User stories covered: 19–23.

## Acceptance criteria

- [x] Exhausting all available Providers enters a Recovery Wait instead of returning the last Failover Failure or a terminal `503`.
- [x] Recovery Wait is configurable and defaults to 30 seconds.
- [x] Completing a Recovery Wait clears all Provider Cooldowns and starts a new Routing Pass in normal priority order.
- [x] A single request can complete multiple Recovery Wait and Routing Pass cycles before eventually succeeding.
- [x] There is no configured or hard-coded attempt limit.
- [x] Cancelling the downstream request promptly stops either an active upstream attempt or a Recovery Wait and prevents further Routing Passes.
- [x] Concurrent recovery activity uses concurrency-safe shared availability state.
- [x] Black-box tests use short deterministic timings to prove repeated recovery, eventual success, and cancellation without waiting for production defaults.

## Blocked by

- [Issue 02: Fail over and Cooldown unhealthy Providers](02-failover-and-cooldown.md)

## Comments

### 2026-08-20

Resolved with configurable Recovery Waits, repeated Routing Passes without an attempt limit, downstream cancellation, concurrency-safe cooldown clearing, and black-box coverage.
