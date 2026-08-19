# Fail over and Cooldown unhealthy Providers

Status: ready-for-agent

## Parent

[Gonka Proxy MVP PRD](../PRD.md)

## What to build

Extend routing so a Failover Failure immediately places the selected Provider in Cooldown and continues the same Routing Pass through remaining available Providers in descending priority order. Provider availability is shared safely across requests. Preserve Client Errors as client-visible responses instead of misclassifying them as Provider instability.

User stories covered: 12–18, 24–25, 28–29.

## Acceptance criteria

- [ ] HTTP `429`, representative HTTP `5xx`, network failure, and response-header timeout each trigger failover to the next available Provider.
- [ ] The response-header timeout is configurable and defaults to 60 seconds.
- [ ] Every Provider is attempted at most once per Routing Pass and attempts follow priority plus declaration-order rules.
- [ ] A Provider enters Cooldown immediately after a Failover Failure; Cooldown is configurable and defaults to 120 seconds.
- [ ] Sequential and concurrent requests skip a Provider whose shared Cooldown is active.
- [ ] A Provider becomes available again when its Cooldown expires normally.
- [ ] HTTP `400`, `401`, `403`, and `404` preserve upstream status, body, and client-relevant headers without failover or Cooldown.
- [ ] A successful lower-priority Provider response is returned transparently after earlier attempts fail.
- [ ] Black-box tests cover every failure category, ordering, shared availability, and Client Error behavior using fake Providers and short test timings.

## Blocked by

None - Issue 01 was resolved in commit `7e6cf76`.
