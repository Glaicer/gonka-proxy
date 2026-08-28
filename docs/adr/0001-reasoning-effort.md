# ADR 0001: Global Reasoning Effort Enforcement

**Status:** Accepted (global-only clause superseded by [ADR 0002](0002-per-provider-reasoning-effort.md); enum restriction superseded by [ADR 0003](0003-reasoning-effort-enum-expansion.md))
**Date:** 2026-08-24

This ADR records the original policy. Its global-only clause is superseded by ADR 0002, and its restricted enum is superseded by ADR 0003; those later decisions are normative for current configurations.

## Context

Upstream providers (OpenAI-compatible) accept a `reasoning_effort` field that controls how much reasoning the model performs. The proxy must ensure every upstream request carries a consistent value regardless of what the client sends, and operators must be able to disable the field entirely. The set of values actually used by our providers is `low`, `high`, and `max` (DeepSeek).

## Decision

1. **Global-only, required field (historical; global-only clause superseded by ADR 0002).** `reasoning_effort` is a top-level key in `config.yaml` (`reasoning_effort: low|high|max|null` at the time). It is **required** — absent key fails validation. `null`/`~` disables forwarding (field is stripped). No per-provider override.

2. **Restricted enum (historical; superseded by ADR 0003).** Only `low`, `high`, `max` (case-insensitive, whitespace-trimmed) plus `null` were accepted. Any other value failed `config.Load` with `reasoning_effort must be one of low, high, max, null`. Empty string was an error.

3. **Overwrite/strip semantics.** Every upstream request is rewritten in a single `applyUpstreamOverrides` step that sets `model` (Model Alias) and enforces `reasoning_effort`: if configured non-null, insert/overwrite the JSON key; if `null`, delete the key even if the client supplied it. This applies to both streaming and non-streaming requests, on every Routing Pass / Retry.

4. **No silent default.** At the time of this ADR, `config.example.yaml` used `max` as its illustrative configured value; the current [config.example.yaml](../../config.example.yaml) uses `xhigh`. The proxy does not default an absent key — it requires an explicit choice so misconfiguration is visible.

## Consequences

- Operators must add `reasoning_effort` to every existing `config.yaml`; old files without it will be rejected at startup.
- Widening the enum required a config validation change; ADR 0003 records the backwards-compatible expansion.
- Per-provider reasoning effort can be added later as an additive optional field without breaking this decision.

## Alternatives Considered

- **Per-provider override** — rejected for MVP complexity; easy to add later.
- **Permissive pass-through (any string)** — rejected; hides typos.
- **Absent → default `max` silently** — rejected; hides misconfiguration. Explicit required field is clearer.
- **Pass-through client value when global is `null`** — rejected; breaks "overwrite" invariant that config is the source of truth.
