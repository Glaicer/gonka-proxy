# Gonka Proxy

**No more 429s. No more 502s.** Gonka Proxy sits in front of your LLM providers and gives you one stable, OpenAI-compatible endpoint to point your app at. If one provider is rate-limited or down, it silently hands the request to the next one — your app never sees the error.

Tiny and cheap: it runs in under **10 MB of RAM** even under load, so you can run it next to anything.

## What it does

- Exposes **one local OpenAI-compatible endpoint** (`/v1/chat/completions`) — your app keeps talking to one URL, no matter what's happening upstream.
- Maintains a **priority-ordered pool of providers** (e.g. primary, then backups).
- On a **402, 429, 404 (optionally) or 5xx** (or a timeout/network error), it **fails over** to the next available provider in order.
- A failed provider goes into a short **cooldown**, then comes back automatically. If every provider is down, it waits and retries until one responds or you cancel.
- Maps one or more **virtual model names** to each provider's real model, so clients don't need to know or care which upstream you're using.
- Enforces one **reasoning effort** setting on every upstream request, with per-provider overrides for backends that don't support the parameter.

## Configuration

Copy the example and edit it:

```sh
cp config.example.yaml config.yaml
```

The file is loaded once at startup, so **any change requires a restart**.

```yaml
server:
  listen_address: 0.0.0.0:8080   # where the proxy listens (host:port)

# Optional tuning — these are the defaults if omitted:
cooldown: 120s            # how long a failed provider is benched before retry
recovery_wait: 30s        # wait before probing again when all providers are down
response_header_timeout: 30s  # max time to wait for a provider's response headers
failover_on_404: false    # set true to try another provider after any upstream 404
log_level: WARN           # INFO, WARN (default), or ERROR

reasoning_effort: xhigh   # required; see "Reasoning effort" below

providers:                # one block per upstream, higher priority = preferred
  - name: primary
    base_url: https://provider.example/v1   # API root, not a /chat/completions URL
    api_key: your-key
    model_alias: provider-model-name        # real model name sent upstream
    models:                                 # optional; upstream models for the benchmark
      - provider-model-name
    priority: 100
  - name: backup
    base_url: https://backup.example/v1
    api_key: your-key
    model_alias: backup-model-name
    priority: 50          # tried only when primary is down or rate-limited
```

List your providers in any order; Gonka always tries the **highest `priority` number first**. Equal priorities keep YAML declaration order. Add as many blocks as you like.

Set `failover_on_404: true` to treat any provider HTTP 404 as a failover failure: the proxy tries the next available provider and places the one that returned 404 in cooldown. The default is `false`, which passes 404 responses back to the client unchanged.

`log_level` is a minimum severity. `INFO` includes detailed lifecycle diagnostics—Provider selection and successes, Cooldown and Recovery Wait transitions, and cancellation—while `WARN` (the default) suppresses those INFO-only events but retains Failover Failure and stream-abort events. Client cancellation before `[DONE]` is logged only at `INFO`; cancellation after `[DONE]` has been forwarded is not logged. Neither affects Provider availability. Upstream read errors and unexpected EOF without `[DONE]` remain stream-abort events. `ERROR` is the strictest threshold and emits only ERROR-level events. At `INFO`, an error may include a bounded provider error message or stream tail; prompts, request bodies, authorization headers, and API keys are never logged, and response content is suppressed at `WARN` and `ERROR`.

### Multiple Virtual Models

For new configurations, define Provider aliases by Virtual Model and declare the Model Route order explicitly:

```yaml
providers:
  - name: gonka24
    base_url: https://provider.example/v1
    api_key: your-key
    model_aliases:
      gonka: deepseek-v4-flash-0731
      deepseek-v4-flash-0731: deepseek-v4-flash-0731
    priority: 100
  - name: easy-gonka
    base_url: https://backup.example/v1
    api_key: your-key
    model_aliases:
      glm-5.3-flash: glm-5.3-flash
    priority: 50

model_routes:
  gonka:
    providers: [gonka24]
  deepseek-v4-flash-0731:
    providers: [gonka24]
  glm-5.3-flash:
    providers: [easy-gonka]
```

The request's `model` selects a Model Route. A missing `model` selects `gonka`; an unknown model returns `404` when `model_routes` is configured. A legacy configuration containing only scalar `model_alias` values continues to route through `gonka` and retains the previous behavior.

Agents can discover the configured Virtual Models through the OpenAI-compatible endpoint:

```sh
curl http://127.0.0.1:58081/v1/models
```

It returns route IDs in configuration order with stable `model` metadata. Provider URLs, credentials, upstream aliases, and temporary Provider health are not exposed.

#### Route fallbacks

A route may point at another route to retry when all its providers fail (server error, cooldown, or blocked/paused key):

```yaml
model_routes:
  gonka:
    providers: [gonka24]
  glm-5.3-flash:
    providers: [easy-gonka]
    fallback: gonka   # if easy-gonka fails, retry on the gonka route
```

Fallback chains are allowed (`fallback` → `fallback`), and cycles or unknown route references are rejected at startup with an actionable error.

## Model limits

Brokers differ in what one model accepts, and `/models` metadata is not something the proxy can read on its own: the numbers live on the upstream, are named differently by each broker (`context_length`, `context_window`, `max_output_tokens`, `max_tokens`, `max_output_length`), and some brokers publish nothing at all. Declare what an upstream actually accepts with `model_limits`, keyed by Virtual Model like `model_aliases`:

```yaml
providers:
  - name: narrow-pool
    # ...
    model_aliases:
      minimax-m2.7: minimaxai/minimax-m2.7
    model_limits:
      minimax-m2.7: {context: 180000, output: 8192}
```

A route's published limit is the **smallest declared value among the providers that serve it**, because failover can land on any of them. A dimension left out of an entry stays unknown rather than being published as zero, and a route where no provider declares it publishes no limit at all.

`/v1/models` then carries `context_length` (plus `context_window`, the name half the Gonka brokers use) and `max_output_tokens` for each route:

```json
{"id": "minimax-m2.7", "object": "model", "created": 0, "owned_by": "gonka-proxy",
 "context_length": 180000, "context_window": 180000, "max_output_tokens": 8192}
```

A provider that declares nothing cannot lower the floor, so a route with such gaps may in fact accept less than the published number. `/diagnostics` therefore lists them per dimension:

```json
{"model": "minimax-m2.7", "context_length": 180000, "max_output_tokens": 8192,
 "context_unknown_providers": ["gonka-router", "easy-gonka", "dahl"],
 "output_unknown_providers": ["hyperfusion", "gonka-router", "easy-gonka"],
 "remediation": "add context to model_limits for minimax-m2.7 on gonka-router, easy-gonka, dahl so the published context accounts for it"}
```

Declare only what is known. A value copied from another provider's `/models` is worse than no value, because it makes the floor look trustworthy. Limits belong in the hot-reload identity, so editing one is noticed on reload.

## Metrics

The proxy exposes Prometheus-compatible metrics at `GET /metrics` on the same address as the chat endpoint. Keep the listener on loopback when metrics must remain local:

```sh
curl http://127.0.0.1:58081/metrics
```

Metrics are grouped by the configured provider name and include successful and failed provider attempts, upstream status/category, failovers, cooldown activations and skips, stream aborts, and latency histograms. The endpoint contains no request bodies, response bodies, authorization headers, or API keys. Useful signals include:

- `gonka_proxy_provider_requests_total` — each request result per provider, labelled by API endpoint (`endpoint="chat/completions"`) and `result`;
- `gonka_proxy_provider_upstream_responses_total` — upstream status and safe failure category;
- `gonka_proxy_provider_failovers_total` and `gonka_proxy_provider_cooldowns_total` — provider failures that caused routing changes;
- `gonka_proxy_provider_stream_aborts_total` — streams cut before the `[DONE]` marker or by a client/upstream read error;
- `gonka_proxy_provider_request_duration_seconds` — completed request latency per provider.

For a human-readable current summary, use `GET /health`:

```sh
curl http://127.0.0.1:58081/health
```

The summary includes each provider's healthy/degraded/unavailable status, last success and error, cooldown transitions, request/error counts, and latency averages. The state is stored in `health_history_path`; writes use an atomic replacement and are best-effort, so an unavailable or read-only path does not stop routing. Events older than 30 days and entries beyond the latest 1,000 are removed.

For active provider diagnostics, use `GET /diagnostics`:

```sh
curl http://127.0.0.1:58081/diagnostics
```

Diagnostics perform a safe `GET` request (by default to `<base_url>/models`) and never send chat messages, prompts, tools, or request bodies. They classify invalid keys, unavailable endpoints, rate/concurrency exhaustion, insufficient balance, and transient failures, with a remediation hint. A provider-specific `balance_url` can be configured when that API exists; otherwise the balance result is reported as `unavailable/unsupported`.

Each report also carries an `observed` block, built only from traffic the proxy already recorded, so it adds no request of its own:

```json
{
  "provider": "gonka24",
  "status": "healthy",
  "observed": {
    "status": "degraded",
    "window_hours": 24,
    "window_start": "2026-09-24T21:10:00Z",
    "successes": 14,
    "errors": 18,
    "error_categories": {"upstream-read": 15, "response-header-timeout": 3},
    "last_success": "2026-09-25T22:58:10Z",
    "last_error": "2026-09-25T23:01:44Z",
    "last_error_category": "upstream-read",
    "cooldown_until": "2026-09-25T23:02:14Z",
    "last_latency_seconds": 1.42,
    "avg_recent_latency_seconds": 2.08
  }
}
```

`observed.status` grades real traffic over the trailing window: `no-data` when nothing was routed to the provider, `healthy` when everything succeeded, `degraded` when both succeeded and failed, `unavailable` when everything failed or the provider is cooling down. Categories are capped to the eight most frequent reasons per provider. Providers that the priority order never selects stay `no-data` — probe them with the [benchmark](#provider-benchmark) instead.

## Provider benchmark

The repository includes a reusable benchmark that tests every model advertised by
each configured provider. It sends requests sequentially, adds a fresh random
nonce to every prompt to reduce cache effects, and records discovery status,
HTTP errors, header/first-byte latency, total latency, and response size. API
keys, prompts, and response bodies are never written to the report.

Run it from the repository root:

```sh
go run ./cmd/provider-benchmark \
  -config config.yaml \
  -rounds 3 \
  -delay 500ms \
  -timeout 45s \
  -output provider-benchmark.json
```

The default run performs three sequential short requests per model. Model
selection is ordered: the provider `/models` endpoint wins when it works,
otherwise the provider's `models` list is used, otherwise the legacy
`model_alias`/`model_aliases` upstream values. The report's `model_source`
records which source won (`endpoint`, `config`, or `legacy`), and a failing
endpoint is still reported through `discovery_status`/`discovery_error` even
when a fallback list is tested.

Use `-provider` and `-model` to narrow a run to comma-separated names; an
unknown provider name aborts before any request is sent, and a model that
matches nothing is simply not tested:

```sh
go run ./cmd/provider-benchmark -config config.yaml -provider gonka-router -model deepseek-v4-flash-0731
```

CSV output is available with `-format csv` and includes the `model_source`
column; use it for comparisons in a spreadsheet or a later analysis script. The
benchmark calls providers directly, so proxy fallback does not hide an
individual provider's result.

## Reasoning effort
`reasoning_effort` hints how much reasoning the upstream model should do. The proxy writes your configured value into every upstream request — whatever the client sends is overwritten. Allowed values: `none`, `low`, `medium`, `high`, `xhigh`, `max`, plus `null` (`~`) to strip the field entirely. The top-level key is **required**.

Not all providers accept the parameter, so each provider block can override it:

```yaml
providers:
  - name: primary
    # ...
    reasoning_effort: high   # optional: use a different value for this provider
  - name: legacy
    # ...
    reasoning_effort: ~      # strip the field entirely for this provider
```

- Omitted on a provider → it inherits the global value.
- A value → overrides the global value for that provider.
- `~` → removes `reasoning_effort` from requests to that provider.

Brokers validate the parameter per model, so one endpoint can serve a reasoning model and a strict one. `model_reasoning_effort` sets the value per Virtual Model, including stripping it for a single model:

```yaml
providers:
  - name: mixed-pool
    # ...
    model_aliases:
      deepseek-v4-flash-0731: deepseek-ai/DeepSeek-V4-Flash-0731
      qwen3.8-27b: Qwen/Qwen3.8-27B
      gemma-4-31b-it: google/gemma-4-31b-it
    model_reasoning_effort:
      qwen3.8-27b: low     # this deployment rejects high/xhigh/max
      gemma-4-31b-it: ~    # this deployment rejects the parameter itself
```

A per-model entry wins over the provider value, which wins over the global one; models without an entry keep the provider-wide value. A `~` entry strips the field for that model only, so the same broker can serve reasoning and non-reasoning models at once.

The same rules apply after failover: each provider always gets its own resolved setting, re-resolved per model.

### Provider support

> ***The data is current as of 2026-09-02. If you would like to add new data or update existing information, please open an issue and I will make the changes.***

Values each provider currently accepts for DeepSeek V4 Flash 0731 model:


| Provider       | low | medium | high | xhigh | max |
| -------------- | --- | ------ | ---- | ----- | --- |
| proxy.gonka.gg | OK  | OK     | OK   | OK    | OK  |
| GonkaRouter    | OK  | OK     | OK   | OK    | OK  |
| GonkaGate      | OK  | OK     | OK   | OK    | OK  |
| Gonka-API      | OK  | OK     | OK   | OK    | OK  |


**Note**: it seems that sometimes `reasoning_effort: max` support depends on devshard/node and might be unstable. Consider adding `reasoning_effort: high` fallbacks to your config.

A provider that does not support the configured value answers with **HTTP 400** and an error like `reasoning_effort: unsupported value: ...`. The proxy detects that message and **fails over** to the next provider, so a single unsupported provider no longer breaks the request. Other 400 responses are still passed back to your app unchanged. To avoid repeated cooldowns, point those providers at a value they accept (or `~` to strip the field) with a per-provider `reasoning_effort`.

To check provider support against your own configuration, run:

```sh
./check-reasoning-effort.sh
```

The script tests every configured provider with all five values, shows progress and an aligned results table.

## Launch

**Docker (recommended):**

```sh
cp config.example.yaml config.yaml   # then edit your providers
docker compose up --build
```

This publishes the proxy on `127.0.0.1:58081` and mounts `config.yaml` read-only. Point your app at `http://127.0.0.1:58081/v1`.

**Multiple instances:** to run several proxies side by side (each with its own providers, cooldowns, and log level), copy `compose.multi.example.yaml` to `compose.yaml` and give each service its own config file and host port:

```yaml
services:
  proxy-a:
    build: .
    command: ["--config", "/etc/gonka-proxy/config.yaml"]
    ports:
      - "127.0.0.1:58081:8080"
    volumes:
      - ./config-a.yaml:/etc/gonka-proxy/config.yaml:ro
  proxy-b:
    build: .
    command: ["--config", "/etc/gonka-proxy/config.yaml"]
    ports:
      - "127.0.0.1:58082:8080"
    volumes:
      - ./config-b.yaml:/etc/gonka-proxy/config.yaml:ro
```

Every service mounts its config at the same in-container path (`/etc/gonka-proxy/config.yaml`) — only the host-side file differs. `config-*.yaml` and `compose.yaml` are gitignored, so per-instance credentials never get committed.

**Native (no Docker):**

```sh
go run ./cmd/gonka-proxy --config config.yaml
```

## Point your app at it

Set your OpenAI-compatible client's `base_url` to `http://127.0.0.1:58081/v1` (or your chosen address) and use one of your `model_alias` values as the model name. The proxy handles the rest.
