# Docker and OpenCode workflow

## Configuration

Copy the documented sample and replace every example Provider value:

```sh
cp config.example.yaml config.yaml
```

Each Provider has a human-readable `name`. Its `base_url` is an OpenAI API root, normally ending in `/v1`. The proxy appends `/chat/completions`, replaces the incoming Virtual Model with `model_alias`, and sends the Provider's `api_key` upstream.

Configuration is loaded and validated once during process startup. Edit the mounted file, then restart the container to apply changes; there is no hot reload.

When omitted, `cooldown` defaults to 120s, `recovery_wait` to 30s, `response_header_timeout` to 30s, `failover_on_404` to `false`, and `log_level` to `WARN`.

## Docker Compose

Start the production image with the loopback-only host binding and read-only configuration mount:

```sh
docker compose up --build
```

The container listens on `0.0.0.0:8080`. Compose publishes it as `127.0.0.1:58081:8080`, so OpenCode can reach it locally without exposing the proxy on the network.

Stop it with `docker compose down`. SIGTERM cancels active upstream routing and pending Recovery Waits before the process exits.

The production image is multi-stage: the final `scratch` image contains only the statically linked service and the public CA certificate bundle. Build tools are present only in the temporary build stage.

## OpenCode-compatible Virtual Model

Use the OpenAI-compatible AI SDK adapter with one stable Virtual Model. The client key is only a placeholder; the proxy ignores it.

```ts
import { createOpenAICompatible } from '@ai-sdk/openai-compatible';

const gonka = createOpenAICompatible({
  name: 'gonka-local',
  baseURL: 'http://127.0.0.1:58081/v1',
  apiKey: 'local-placeholder',
});

export const model = gonka('gonka-virtual');
```

The `baseURL` intentionally ends in `/v1`. Do not configure individual upstream Provider URLs in OpenCode.

## Operational logs

Logs are written to the container console with timestamps. `INFO` includes detailed lifecycle diagnostics—Provider selection and successes, Cooldown and Recovery Wait transitions, and cancellation. `WARN` (the default) suppresses those INFO-only events but retains Failover Failure and stream-abort events. `ERROR` is the strictest threshold and emits only ERROR-level events. At `INFO`, an error may include a bounded parsed Provider error message or stream tail that can contain Provider response content; this content is suppressed at `WARN` and `ERROR`. Prompts, request bodies, downstream authorization headers, and Provider API keys are never logged.

## Metrics

The same server exposes Prometheus-compatible metrics at `GET /metrics`. With the compose example, query them locally:

```sh
curl http://127.0.0.1:58081/metrics
```

The metrics report per-Provider successes and errors, upstream status/category, failovers, cooldowns, stream aborts, and latency histograms. They do not include request or response bodies, authorization headers, or API keys. Keep the host binding loopback-only if metrics should not be reachable from the network.

For a human-readable provider summary, query `GET /health`. It reports current status, last success/error, cooldown transitions, counts, and latency averages. Health history is persisted at `health_history_path` with a 30-day/1,000-event retention limit. Persistence is best-effort: a broken path is logged but does not interrupt proxy routing.

For active diagnostics, query `GET /diagnostics`. The proxy sends only authenticated `GET` checks to each provider (default `<base_url>/models`), never chat prompts or tools. Configure an optional provider `balance_url` for upstreams that expose a balance API; omitted capability is reported as `unavailable/unsupported`. Every report also includes an `observed` block summarizing the last 24 hours of recorded traffic (successes, failures, error categories, cooldown, latency) straight from local history, so it costs no extra upstream request.

## Container smoke test

The smoke test builds the production image and a fake HTTPS OpenAI-compatible Provider, publishes the proxy on the host loopback interface, then sends a real request through that published endpoint:

```sh
sh scripts/smoke-test.sh
```

The fake Provider verifies the upstream bearer credential and the Model Alias before returning a completion-shaped response.
