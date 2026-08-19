# Gonka Proxy MVP

Status: ready-for-agent

## Problem Statement

OpenCode can use Gonka AI providers through OpenAI-compatible endpoints, but individual providers intermittently return `429`, `5xx`, time out, or become unreachable. The user currently has to watch OpenCode and manually intervene when this happens. They need inference requests to continue automatically through other configured providers without changing their OpenCode workflow or babysitting long-running sessions.

## Solution

Build a lightweight Go HTTP proxy that exposes an OpenAI-compatible `/v1/chat/completions` endpoint to OpenCode. The proxy always starts with the highest-priority available Provider, replaces the incoming Virtual Model with that Provider's Model Alias, and forwards the request using the Provider's API key.

When a Provider produces a Failover Failure, the proxy places it in Cooldown and continues the Routing Pass with the next available Provider. If no Providers remain available, the request enters a Recovery Wait, clears all Cooldowns when the wait ends, and starts another Routing Pass. This continues without an attempt limit until a Provider succeeds or OpenCode cancels the request.

The service runs in Docker, is exposed only on the host loopback interface, requires no client authentication, and is configured through a mounted YAML file.

## User Stories

1. As an OpenCode user, I want to configure one local OpenAI-compatible endpoint, so that I do not manage multiple Provider endpoints in OpenCode.
2. As an OpenCode user, I want the proxy to accept `/v1/chat/completions` requests, so that I can use `@ai-sdk/openai-compatible` without changing its protocol.
3. As an OpenCode user, I want streaming chat completions to pass through incrementally, so that interactive responses remain responsive.
4. As an OpenCode user, I want requests to start with the highest-priority available Provider, so that my preferred Provider is used whenever healthy.
5. As an operator, I want each Provider to have an integer priority, so that routing preference is explicit.
6. As an operator, I want Providers with equal priority to follow their YAML declaration order, so that routing is deterministic.
7. As an operator, I want to configure each Provider's base URL, API key, Model Alias, and priority, so that heterogeneous OpenAI-compatible endpoints can participate in one pool.
8. As an OpenCode user, I want the incoming Virtual Model replaced with the selected Provider's Model Alias, so that OpenCode can use one stable model identifier.
9. As an operator, I want the selected Provider's API key sent upstream, so that each Provider authenticates independently.
10. As an OpenCode user, I want to call the proxy without an API key, so that local setup is minimal.
11. As an OpenCode user, I want any client-supplied API key to be accepted and ignored, so that OpenCode configurations that require a placeholder key still work.
12. As an OpenCode user, I want a `429` response to trigger failover, so that rate-limited Providers do not interrupt my session.
13. As an OpenCode user, I want a `5xx` response to trigger failover, so that transient Provider outages do not interrupt my session.
14. As an OpenCode user, I want connection failures and DNS/TLS/network errors to trigger failover, so that unreachable Providers are skipped automatically.
15. As an OpenCode user, I want a Provider that does not return response headers within the configured timeout to trigger failover, so that a hung connection cannot stall routing forever.
16. As an operator, I want a Failover Failure to put the Provider in Cooldown, so that subsequent requests do not repeatedly hit a known-unhealthy Provider.
17. As an operator, I want the Cooldown duration configurable, so that I can tune recovery behavior; its default is 120 seconds.
18. As an OpenCode user, I want a Routing Pass to continue through available Providers in descending priority order, so that one transient failure does not become a user-visible failure.
19. As an OpenCode user, I want the proxy to wait when all Providers are in Cooldown, so that temporary total outages recover without manual intervention.
20. As an operator, I want the Recovery Wait configurable, so that I can tune how soon the pool is retried; its default is 30 seconds.
21. As an OpenCode user, I want all Cooldowns cleared after a Recovery Wait, so that every Provider gets another chance in the next Routing Pass.
22. As an OpenCode user, I want Recovery Waits and Routing Passes to repeat without an attempt limit, so that I do not have to babysit OpenCode during an extended outage.
23. As an OpenCode user, I want cancellation of my request to stop upstream work and pending waits, so that abandoned requests consume no ongoing resources.
24. As an OpenCode user, I want upstream `400`, `401`, `403`, and `404` Client Errors returned with their original status and body, so that OpenCode can recognize and display the actual problem.
25. As an operator, I want Client Errors not to trigger failover or Cooldown, so that invalid requests and credentials are not misdiagnosed as Provider instability.
26. As an OpenCode user, I want successful response status, headers, body, and stream semantics preserved, so that the proxy remains transparent to OpenCode.
27. As an OpenCode user, I want an interrupted stream not to be mixed with a second Provider's generation, so that I never receive a corrupt combined response.
28. As an operator, I want a configurable response-header timeout with a 60-second default, so that slow startup and streaming duration are controlled separately.
29. As an operator, I want Cooldown state shared across concurrent requests, so that a failure discovered by one request protects all subsequent requests.
30. As an operator, I want the proxy to load a mounted YAML file at startup, so that Providers and timing values can be changed without rebuilding the image.
31. As an operator, I want invalid or empty Provider configuration to fail startup with a clear error, so that the service never runs in a nonfunctional state.
32. As an operator, I want configuration changes to take effect after container restart, so that the MVP has predictable configuration lifecycle semantics.
33. As an operator, I want concise logs of routing attempts, response classes, Cooldowns, and Recovery Waits, so that I can diagnose Provider health.
34. As an operator, I want logs to exclude request bodies and API keys, so that prompts and credentials are not leaked.
35. As an operator, I want a small single-service Docker image, so that the proxy has low memory, CPU, storage, and operational overhead.
36. As an operator, I want the container to listen on its internal interface while Docker publishes it only to `127.0.0.1`, so that OpenCode can reach it without exposing it to the network.
37. As an operator, I want HTTPS Provider endpoints to work using normal public certificate authorities, so that no extra TLS setup is required for Gonka providers.
38. As an operator, I want the service to shut down cleanly and cancel active routing when the container stops, so that restart and maintenance are predictable.

## Implementation Decisions

- Implement the service in Go to keep the runtime small while using the standard HTTP stack for cancellation, streaming, and connection reuse.
- Expose only the OpenAI-compatible `POST /v1/chat/completions` inference contract required by the OpenCode `@ai-sdk/openai-compatible` integration.
- Accept streaming and non-streaming JSON request bodies. Retain a replayable copy of the incoming request for each Routing Pass and replace its top-level `model` field with the selected Provider's Model Alias before every upstream attempt.
- Treat HTTP `429`, HTTP `5xx`, failure to receive response headers within the configured timeout, and network-level failures as Failover Failures.
- Treat HTTP `4xx` other than `429` as Client Errors. Return their upstream status and body to OpenCode without trying another Provider or changing Provider availability. Preserve response headers relevant to the client contract, excluding hop-by-hop headers.
- Once response headers and body streaming have begun, do not attempt failover. If the upstream stream later ends unexpectedly, close the downstream stream and place the Provider in Cooldown; OpenCode decides whether to repeat the whole request.
- Maintain Provider availability as process-wide, concurrency-safe in-memory state. A process restart clears all Cooldowns.
- Start each Routing Pass with available Providers sorted by descending integer priority. Preserve YAML declaration order when priorities are equal. Try each Provider at most once within a Routing Pass.
- After a Failover Failure, record the Provider's Cooldown immediately so new requests skip it.
- When no Provider is available, wait for the configured Recovery Wait, clear every Provider's Cooldown, and start a new Routing Pass. Repeat until success or downstream cancellation; do not return a terminal exhaustion error.
- Propagate downstream request cancellation through active upstream attempts and Recovery Waits.
- Use a YAML configuration containing server listen address, Cooldown duration, Recovery Wait duration, response-header timeout, and an ordered Provider list. Each Provider requires `base_url`, `api_key`, `model_alias`, and `priority`.
- Default Cooldown to 120 seconds, Recovery Wait to 30 seconds, response-header timeout to 60 seconds, and the container listen address to `0.0.0.0:8080`.
- Interpret a Provider base URL as its OpenAI API root, normally ending in `/v1`, and append the chat-completions route without duplicating path segments.
- Ignore missing or arbitrary downstream authorization credentials. Replace any downstream authorization header with the selected Provider's bearer credential upstream.
- Load and validate configuration only at process startup. Reject malformed durations, missing required Provider fields, duplicate or unusable Provider definitions, and an empty Provider list.
- Use connection pooling and stream response bodies instead of buffering successful completions. Do not log request bodies, response bodies, authorization headers, or API keys.
- Log Provider selection, Failover Failure category, status code when present, Cooldown transitions, Recovery Wait transitions, and cancellation at a concise operational level.
- Build a minimal production container that includes trusted CA certificates and the compiled Go binary. Provide a Docker run/Compose example that binds host `127.0.0.1` to the container port and mounts the YAML configuration read-only.
- Include an OpenCode configuration example using a local base URL ending in `/v1`, `@ai-sdk/openai-compatible`, one Virtual Model, and either no API key or an arbitrary placeholder key.

## Testing Decisions

- The primary test seam is the public HTTP API. Tests start the proxy with temporary YAML configuration and local fake upstream Providers, then issue real OpenAI-compatible requests through `/v1/chat/completions`.
- Prefer externally observable assertions: upstream attempt order, received Model Alias and authorization, downstream status/headers/body, streamed chunks, elapsed routing state, and cancellation. Do not test private functions or internal data structures directly.
- Verify that the highest-priority available Provider handles a successful request and equal priorities respect configuration order.
- Verify failover independently for `429`, representative `5xx`, response-header timeout, and network failure.
- Verify a failed Provider is skipped by later concurrent and sequential requests until its Cooldown is cleared.
- Verify `400`, `401`, `403`, and `404` are returned unchanged and cause neither another upstream attempt nor Cooldown.
- Verify the incoming Virtual Model is replaced by each attempted Provider's Model Alias while the rest of the request remains semantically unchanged.
- Verify client authorization is optional or arbitrary and that each upstream receives only its configured Provider API key.
- Verify streaming begins promptly and preserves chunks without buffering the completed response.
- Verify an upstream failure after streaming starts does not invoke a second Provider and closes the stream.
- Verify exhaustion enters Recovery Wait, clears all Cooldowns, and begins another Routing Pass in priority order.
- Verify multiple Recovery Wait cycles can occur before eventual success, without a configured attempt cap.
- Verify downstream cancellation interrupts both an in-flight upstream request and a Recovery Wait promptly.
- Verify configuration startup behavior separately through the process boundary: valid YAML starts the service, while malformed YAML, invalid durations, missing required fields, and an empty Provider list fail with actionable errors.
- Verify graceful container/process shutdown cancels active work.
- Use short injected timing values in tests rather than the production defaults, keeping tests deterministic and fast.
- There is no prior implementation or test suite in the repository. The first implementation establishes the black-box HTTP test pattern; internal unit tests should be added only where behavior cannot be observed reliably through that seam.

## Out of Scope

- OpenAI endpoints other than `POST /v1/chat/completions`, including `/v1/responses`, embeddings, images, audio, and model discovery.
- Dynamic configuration reload or an administrative API/UI.
- Client authentication, authorization, quotas, tenants, or rate limiting.
- Provider health checks, active probing, adaptive scoring, weighted load balancing, or latency-based routing.
- Persistent or distributed Cooldown state across process replicas or restarts.
- Multiple proxy replicas coordinating Provider state.
- Retrying or merging a response after streaming has started.
- Request/response transformation beyond replacing the top-level model and upstream authorization.
- Prompt logging, response logging, metrics backends, tracing systems, dashboards, or alerting integrations.
- Custom Provider TLS certificates, mutual TLS, HTTP proxies, or Provider-specific protocols.
- Hot-reloadable secrets, external secret managers, or environment-variable interpolation in the YAML file.
- Formal protection for unusually large request bodies or other nonstandard OpenAI-compatible behavior in the MVP.

## Further Notes

- Gonka Proxy is intentionally optimized for unattended continuity rather than fast terminal failure. During a total Provider outage, a request remains open and continues Recovery Wait cycles until OpenCode cancels it.
- Docker loopback exposure is achieved by host port publishing such as `127.0.0.1:8080:8080`; binding only to `127.0.0.1` inside the container would prevent normal host port forwarding.
- The OpenCode provider must use `@ai-sdk/openai-compatible`; current OpenCode documentation identifies this package with the `/v1/chat/completions` contract.
