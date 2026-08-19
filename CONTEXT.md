# Gonka Proxy

Gonka Proxy routes OpenAI-compatible inference requests across prioritized providers while preserving the client-facing API contract.

## Language

**Provider**:
An OpenAI-compatible inference endpoint configured with a base URL, API key, model alias, and priority.

**Virtual Model**:
The model name sent by the client. It identifies the proxy configuration rather than an upstream model and is not forwarded to providers.

**Model Alias**:
The provider-specific model name that replaces the Virtual Model in every upstream inference request.
_Avoid_: Model, virtual model

**Failover Failure**:
A provider response or connection failure that permits retrying another provider and temporarily makes the failed provider unavailable. This includes HTTP `429`, HTTP `5xx`, timeouts, and network errors.
_Avoid_: Any non-200 response, provider error

**Client Error**:
An HTTP `4xx` response other than `429` that is returned to the client unchanged and does not affect provider availability.
_Avoid_: Provider failure

**Cooldown**:
A configured period during which a provider that produced a Failover Failure is excluded from routing.
_Avoid_: Ban, disablement

**Routing Pass**:
One attempt to obtain a successful response by trying every available provider in descending priority order.
_Avoid_: Retry

**Recovery Wait**:
A configured delay entered when no providers remain available. When it ends, all cooldowns are cleared and a new Routing Pass begins; this repeats until success or client cancellation.
_Avoid_: Retry limit, terminal failure
