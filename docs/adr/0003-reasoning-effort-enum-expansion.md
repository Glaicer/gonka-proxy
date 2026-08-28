# ADR 0003: Reasoning Effort Enum Expansion

**Status:** Accepted
**Date:** 2026-08-28
**Supersedes (in part):** [ADR 0001](0001-reasoning-effort.md) and [ADR 0002](0002-per-provider-reasoning-effort.md) — their restricted enum clauses

## Context

ADR 0001 established a required global `reasoning_effort` setting, and ADR 0002 added optional per-Provider overrides. Both originally restricted values to `low`, `high`, and `max`, plus YAML `null`. The supported upstream set has since expanded, so the same validation rule must accept the additional `none`, `medium`, and `xhigh` values in both scopes.

## Decision

1. **Expanded enum.** Global and per-Provider `reasoning_effort` values accept `none`, `low`, `medium`, `high`, `xhigh`, and `max`. Values are case-insensitive and surrounding whitespace is ignored.

2. **YAML null remains supported.** An unquoted YAML `null`/`~` disables forwarding for the applicable scope. The quoted string `"null"` is not a value in the enum and remains invalid.

3. **Existing scope and resolution rules remain.** The top-level key is required. A Provider-level value overrides the global value, an explicit Provider-level null strips the field, and an omitted Provider-level key inherits the global value.

4. **Overwrite/strip semantics remain.** The resolved setting overwrites any client-supplied `reasoning_effort` on every upstream request; a resolved null removes the field.

## Consequences

- Configurations can select any of the six supported non-null effort values without validation failure.
- Global and per-Provider validation use the same expanded enum and strict normalization rules.
- The required global key, per-Provider inheritance, explicit null stripping, and client-value overwrite guarantees from ADR 0001 and ADR 0002 remain in force.
