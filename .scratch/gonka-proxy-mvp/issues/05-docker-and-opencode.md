# Run Gonka Proxy safely in Docker with OpenCode

Status: ready-for-agent

## Parent

[Gonka Proxy MVP PRD](../PRD.md)

## What to build

Package the completed proxy as a small production container and document the complete local OpenCode workflow. Harden the process boundary with actionable startup validation, safe operational logs, and graceful shutdown while preserving the resource-light MVP design.

User stories covered: 30–38.

## Acceptance criteria

- [ ] Malformed YAML, invalid durations, missing required Provider fields, duplicate or unusable Provider definitions, and an empty Provider list each fail startup with an actionable error.
- [ ] Configuration is loaded once at startup and a documented restart is required to apply changes.
- [ ] Operational logs identify Provider selection, Failover Failure category and status when present, Cooldown transitions, Recovery Wait transitions, and cancellation.
- [ ] Logs never contain request bodies, response bodies, downstream authorization, or Provider API keys.
- [ ] Process termination stops accepting work, cancels active routing and waits, and exits cleanly.
- [ ] A multi-stage production image contains the Go service and trusted public CA certificates without build tooling.
- [ ] The container listens on `0.0.0.0:8080`, while the documented Docker/Compose configuration publishes it only on host `127.0.0.1` and mounts YAML read-only.
- [ ] A documented sample YAML includes timing settings and multiple prioritized Providers without embedding real credentials.
- [ ] A documented OpenCode example uses a localhost base URL ending in `/v1`, `@ai-sdk/openai-compatible`, one Virtual Model, and no meaningful client credential.
- [ ] An automated smoke test or equivalent validation proves a request can travel from the host-facing container endpoint to a fake HTTPS-compatible Provider contract.
- [ ] Relevant Go tests and static checks pass, and the production image builds successfully.

## Blocked by

- [Issue 02: Fail over and Cooldown unhealthy Providers](02-failover-and-cooldown.md)
- [Issue 03: Stream Provider responses without corrupting generations](03-stream-provider-responses.md)
- [Issue 04: Recover indefinitely until success or client cancellation](04-unbounded-recovery.md)
