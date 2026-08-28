# ADR 0002: Per-Provider Reasoning Effort

**Status:** Accepted (enum restriction superseded by [ADR 0003](0003-reasoning-effort-enum-expansion.md))
**Date:** 2026-08-25
**Supersedes (in part):** [ADR 0001](0001-reasoning-effort.md) — the "global-only" clause; its enum restriction, and the restriction repeated here, are superseded by [ADR 0003](0003-reasoning-effort-enum-expansion.md). All other decisions in ADR 0001 stand.

This ADR records the original per-Provider policy. Its restricted enum is historical; ADR 0003 is normative for the current accepted values.

## Context

Not every upstream provider accepts the OpenAI-compatible `reasoning_effort` field. Under ADR 0001, a globally configured value is written into every upstream request, which breaks providers that reject unknown parameters. Operators therefore need a way to exempt individual providers — or tune them individually — without giving up the global overwrite/strip invariant.

## Decision

1. **Optional per-provider key.** Each provider may set `reasoning_effort` in its config entry with the same enum as the global field (`low|high|max|null` at the time). The key is absent by default and old configs remain valid.

2. **Resolution order.** Per provider: an explicit `null` strips the field for that provider; otherwise a configured value overrides the global one; otherwise the provider inherits the global value. Resolution happens once at proxy construction, so routing applies it identically on every Routing Pass.

3. **Same restricted enum, same strictness (historical; superseded by ADR 0003).** Provider values were case-insensitive and whitespace-trimmed like the global value; quoted `"null"` and empty strings were errors, reported as `providers[N].reasoning_effort must be one of low, high, max, null`.

4. **Global stays required.** The top-level `reasoning_effort` remains mandatory; per-provider entries only override or exempt.

## Consequences

- Providers that reject `reasoning_effort` can be opted out with `reasoning_effort: ~` on their config entry.
- `applyUpstreamOverrides` is unchanged; each provider now carries its resolved effort from startup.
- The enum restriction here is superseded by ADR 0003; both scopes now use its expanded value set.

## Alternatives Considered

- **Drop the global field entirely and require per-provider values** — rejected; most deployments want one value everywhere and the global default documents intent.
- **Pass-through of client-supplied `reasoning_effort` for exempt providers** — rejected; breaks the invariant that config is the source of truth.
