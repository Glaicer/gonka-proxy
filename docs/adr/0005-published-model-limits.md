# ADR 0005: Published Model Limits

**Status:** Accepted
**Date:** 2026-09-26
**Extends:** [ADR 0004](0004-per-model-reasoning-effort.md) (per-model settings keyed by Virtual Model). The granularity rationale stands.

## Context

A context window and a generation cap belong to one model at one provider, not to a model name. Measured across the same pool serving one Virtual Model, `minimax-m2.7` ranged from 180000 to 204800 tokens of context and 8192 to 16384 of output, and `deepseek-v4-flash-0731` from 200000 to 1048576.

The proxy fronts that pool, so `/v1/models` had to answer for a route whose request may land on any of its providers. It published only `id`, `object`, `created`, and `owned_by`, which left every client to guess: an operator copying a number from one broker's `/models` into a client config published a claim about a different path entirely.

The proxy cannot discover these numbers itself. It would have to call every provider's `/models` on the request path, which breaks the rule that `/v1/models` and `/diagnostics` add no upstream traffic of their own, and one HTTP call cannot discover a cap that a broker omits or misreports. Concretely, one gateway in the pool answers `/models` with 403, three publish no limits, and one reports `max_output_tokens` equal to its `max_total_tokens` — a total budget, not a generation cap.

## Decision

1. **Declared, not discovered.** A Provider may set `model_limits`, keyed by Virtual Model like `model_aliases`, with a positive `context` and/or `output`. An omitted dimension means the provider did not declare it, and is not published as zero. An entry for a model the Provider does not serve is a startup error, since it cannot affect any floor and only hides a typo.

2. **The floor is the minimum over the route.** A route publishes the smallest declared `context` and the smallest declared `output` among the providers that serve it, because failover can select any of them. A provider outside the route never lowers it.

3. **Silence is reported, not smoothed over.** A silent provider cannot lower the floor, so the published number alone can look more trustworthy than it is. `/diagnostics` therefore lists, per dimension, which providers serving a route declared nothing, with a remediation hint. Tracking the two dimensions separately matters: a provider may publish a context window and say nothing about generation.

4. **Publication shape.** `/v1/models` adds `context_length` plus `context_window` (the name used by half the Gonka brokers) and `max_output_tokens`, all omitted when unknown. `context_length` is the OpenRouter-style name; the duplicate is deliberate compatibility with the brokers this proxy fronts.

5. **Part of Provider identity.** Limits join the hot-reload identity, so editing one is noticed without a restart.

## Consequences

- A client can size its context from `/v1/models` instead of guessing, and the number is guaranteed by the whole route rather than by one lucky provider.
- The published floor is only as good as the operator's declarations; a number copied from another provider is worse than none, because it looks verified. `/diagnostics` is where that gap is visible.
- Declaring a limit is manual work per provider per model, and it goes stale silently as brokers resize. There is no warning when an upstream shrinks below a declared value.
- A declared limit does not enforce anything: an over-long request still fails upstream. This is discovery, not truncation.

## Alternatives Considered

- **Query every provider's `/models` per request** — rejected; adds upstream traffic to a discovery endpoint, and still returns nothing for the providers that stay silent.
- **Publish the maximum across the route** — rejected; it advertises a window the route cannot honour whenever failover reaches the smallest provider.
- **Publish nothing and leave clients to configure limits themselves** — rejected; that is the status quo this decision exists to fix, and it is how a wrong number got into a client config in the first place.
- **Truncate or reject over-long requests at the proxy** — out of scope; worth considering separately, since it turns a declaration into an enforced bound.
