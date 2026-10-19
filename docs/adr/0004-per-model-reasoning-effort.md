# ADR 0004: Per-Model Reasoning Effort

**Status:** Accepted (granularity extended by [ADR 0005](0005-published-model-limits.md))
**Date:** 2026-09-26
**Extends:** [ADR 0002](0002-per-provider-reasoning-effort.md) (per-Provider effort) and [ADR 0003](0003-reasoning-effort-enum-expansion.md) (current enum). Their decisions stand.

## Context

Brokers validate `reasoning_effort` per model, not per endpoint. One provider was observed accepting `high` for `deepseek-ai/DeepSeek-V4-Flash-0731` while rejecting it for `Qwen/Qwen3.8-27B` (`supported types are xhigh (default), medium, and low`) and rejecting the parameter itself for `google/gemma-4-31b-it` (`openai does not support parameters: ['reasoning_effort']`). Both failures arrive as HTTP 400, and the first has no alternative Provider for those models, so failover cannot recover.

ADR 0002 resolves effort once per Provider at construction, which cannot express "this endpoint serves three models with three different rules". Stripping the field for the whole Provider would fix the two strict models but disable effort for the reasoning model that Provider is preferred for.

## Decision

1. **Optional per-model map.** A Provider may set `model_reasoning_effort`, keyed by Virtual Model. Values use the ADR 0003 enum; an explicit `null` (`~`) strips the field for that model alone. Absent by default, so old configs stay valid.

2. **Resolution order.** Per model: a per-model entry wins — a value overrides, `null` strips; a model without an entry keeps the Provider's resolved ADR 0002 value, which is still the per-Provider override or the global value. The global key stays required.

3. **Resolution point.** The lookup happens per Routing Pass, keyed by the Virtual Model already used to resolve the upstream alias, so failover re-resolves for every candidate Provider.

4. **No cross-checking against configured models.** A key that no route serves is inert rather than an error, because `model_routes` and a Provider's aliases are validated separately and the benchmark is free to probe models that are not routed.

5. **Part of Provider identity.** All effort settings feed the hot-reload identity, so changing a per-model effort alone is noticed.

## Consequences

- One broker can serve reasoning and non-reasoning models without disabling effort for either.
- A wrong per-model value surfaces as the upstream's own HTTP 400, unchanged from today; `null` is the escape hatch.
- Config authors must know each deployment's rules, which is broker knowledge the proxy cannot infer without probing.

## Alternatives Considered

- **Strip the field globally (`reasoning_effort: ~`)** — rejected; works, but silently disables effort control for the reasoning models that dominate the pool.
- **Drop the two strict models from the routes** — rejected; loses working models to work around a configuration granularity gap.
- **Probe each model's accepted values at startup** — rejected; sends chat requests the proxy otherwise never makes, and the accepted set changes under load.
- **Widen the failover detector to catch "Unexpected reasoning effort"** — kept as a separate concern; it helps only when another Provider serves the model, so it does not remove the need for this key.
