# Gonka Proxy

**No more 429s. No more 502s.** Gonka Proxy sits in front of your LLM providers and gives you one stable, OpenAI-compatible endpoint to point your app at. If one provider is rate-limited or down, it silently hands the request to the next one — your app never sees the error.

Tiny and cheap: it runs in under **10 MB of RAM** even under load, so you can run it next to anything.

## What it does

- Exposes **one local OpenAI-compatible endpoint** (`/v1/chat/completions`) — your app keeps talking to one URL, no matter what's happening upstream.
- Maintains a **priority-ordered pool of providers** (e.g. primary, then backups).
- On a **402, 429 or 5xx** (or a timeout/network error), it **fails over** to the next available provider in order.
- A failed provider goes into a short **cooldown**, then comes back automatically. If every provider is down, it waits and retries until one responds or you cancel.
- Maps your **virtual model name** to each provider's real model, so clients don't need to know or care which upstream you're using.
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
log_level: WARN           # INFO, WARN (default), or ERROR

reasoning_effort: xhigh   # required; see "Reasoning effort" below

providers:                # one block per upstream, higher priority = preferred
  - name: primary
    base_url: https://provider.example/v1   # API root, not a /chat/completions URL
    api_key: your-key
    model_alias: provider-model-name        # real model name sent upstream
    priority: 100
  - name: backup
    base_url: https://backup.example/v1
    api_key: your-key
    model_alias: backup-model-name
    priority: 50          # tried only when primary is down or rate-limited
```

List your providers in any order; Gonka always tries the **highest `priority` number first**. Equal priorities keep YAML declaration order. Add as many blocks as you like.

`log_level` is a minimum severity. `INFO` includes detailed lifecycle diagnostics—Provider selection and successes, Cooldown and Recovery Wait transitions, and cancellation—while `WARN` (the default) suppresses those INFO-only events but retains Failover Failure and stream-abort events. `ERROR` is the strictest threshold and emits only ERROR-level events. At `INFO`, an error may include a bounded provider error message or stream tail; prompts, request bodies, authorization headers, and API keys are never logged, and response content is suppressed at `WARN` and `ERROR`.

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

The same rules apply after failover: each provider always gets its own resolved setting.

### Provider support

> ***The data is current as of 2026-08-25. If you would like to add new data or update existing information, please open an issue and I will make the changes.***

Values each provider currently accepts for DeepSeek V4 Flash 0731 model:


| Provider       | low | medium | high | xhigh | max         |
| -------------- | --- | ------ | ---- | ----- | ----------- |
| proxy.gonka.gg | OK  | OK     | OK   | OK    | OK          |
| GonkaRouter    | OK  | OK     | OK   | OK    | unsupported |
| GonkaGate      | OK  | OK     | OK   | OK    | OK |
| Gonka-API      | OK  | OK     | OK   | OK    | unsupported |


An unsupported value makes the provider answer with **HTTP 400**, which is passed back to your app unchanged — it does **not** fail over. Point those providers at a value they accept (or `~` to strip the field) with a per-provider `reasoning_effort`.

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

**Native (no Docker):**

```sh
go run ./cmd/gonka-proxy --config config.yaml
```

## Point your app at it

Set your OpenAI-compatible client's `base_url` to `http://127.0.0.1:58081/v1` (or your chosen address) and use one of your `model_alias` values as the model name. The proxy handles the rest.
