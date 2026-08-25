# ADR 0001: Global Reasoning Effort Enforcement

**Status:** Accepted (global-only clause superseded by [ADR 0002](0002-per-provider-reasoning-effort.md))
**Date:** 2026-08-24

## Context

Upstream providers (OpenAI-compatible) accept a `reasoning_effort` field that controls how much reasoning the model performs. The proxy must ensure every upstream request carries a consistent value regardless of what the client sends, and operators must be able to disable the field entirely. The set of values actually used by our providers is `low`, `high`, and `max` (DeepSeek).

## Decision

1. **Global-only, required field.** `reasoning_effort` is a top-level key in `config.yaml` (`reasoning_effort: low|high|max|null`). It is **required** — absent key fails validation. `null`/`~` disables forwarding (field is stripped). No per-provider override.

2. **Restricted enum.** Only `low`, `high`, `max` (case-insensitive, whitespace-trimmed) plus `null` are accepted. Any other value fails `config.Load` with `reasoning_effort must be one of low, high, max, null`. Empty string is an error.

3. **Overwrite/strip semantics.** Every upstream request is rewritten in a single `applyUpstreamOverrides` step that sets `model` (Model Alias) and enforces `reasoning_effort`: if configured non-null, insert/overwrite the JSON key; if `null`, delete the key even if the client supplied it. This applies to both streaming and non-streaming requests, on every Routing Pass / Retry.

4. **No silent default.** The documented default is `max` in `config.example.yaml`, but the proxy does not default an absent key — it requires an explicit choice so misconfiguration is visible.

## Consequences

- Operators must add `reasoning_effort` to every existing `config.yaml`; old files without it will be rejected at startup.
- Adding values like `minimal`/`medium` later requires a config validation change (breaking enum widening, but backwards compatible).
- Per-provider reasoning effort can be added later as an additive optional field without breaking this decision.

## Alternatives Considered

- **Per-provider override** — rejected for MVP complexity; easy to add later.
- **Permissive pass-through (any string)** — rejected; hides typos.
- **Absent → default `max` silently** — rejected; hides misconfiguration. Explicit required field is clearer.
- **Pass-through client value when global is `null`** — rejected; breaks "overwrite" invariant that config is the source of truth.
