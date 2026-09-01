# Gonka Proxy

Gonka Proxy routes OpenAI-compatible inference requests across prioritized providers while preserving the client-facing API contract.

## Language

**Provider**:
An OpenAI-compatible inference endpoint configured with a human-readable name, base URL, API key, model alias, and priority.

**Virtual Model**:
The model name sent by the client. It identifies the proxy configuration rather than an upstream model and is not forwarded to providers.

**Model Alias**:
The provider-specific model name that replaces the Virtual Model in every upstream inference request.
_Avoid_: Model, virtual model

**Failover Failure**:
A provider response or connection failure that permits retrying another provider and temporarily makes the failed provider unavailable. This includes HTTP `402`, HTTP `429`, HTTP `5xx`, timeouts, network errors, and an HTTP `400` whose error message names `reasoning_effort` as an unsupported value.
_Avoid_: Any non-200 response, provider error

**Client Error**:
An HTTP `4xx` response other than `402` or `429` that is returned to the client unchanged and does not affect provider availability. A `400` naming `reasoning_effort` as an unsupported value is a Failover Failure instead.
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

**Reasoning Effort**:
An OpenAI-compatible request field (`reasoning_effort`) that hints how much reasoning the upstream model should do. The proxy enforces a globally configured value on every upstream request, overwriting any client-supplied value; when configured as `null` the field is stripped from the upstream payload. A provider may override the global value with its own effort, or strip the field entirely with an explicit null, for providers that reject the parameter.
_Avoid_: reasoning level, thinking budget
