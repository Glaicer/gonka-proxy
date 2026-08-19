# Docker and OpenCode workflow

## Configuration

Copy the documented sample and replace every example Provider value:

```sh
cp config.example.yaml config.yaml
```

Each `base_url` is an OpenAI API root, normally ending in `/v1`. The proxy appends `/chat/completions`, replaces the incoming Virtual Model with `model_alias`, and sends the Provider's `api_key` upstream.

Configuration is loaded and validated once during process startup. Edit the mounted file, then restart the container to apply changes; there is no hot reload.

## Docker Compose

Start the production image with the loopback-only host binding and read-only configuration mount:

```sh
docker compose up --build
```

The container listens on `0.0.0.0:8080`. Compose publishes it as `127.0.0.1:8080:8080`, so OpenCode can reach it locally without exposing the proxy on the network.

Stop it with `docker compose down`. SIGTERM cancels active upstream routing and pending Recovery Waits before the process exits.

The production image is multi-stage: the final `scratch` image contains only the statically linked service and the public CA certificate bundle. Build tools are present only in the temporary build stage.

## OpenCode-compatible Virtual Model

Use the OpenAI-compatible AI SDK adapter with one stable Virtual Model. The client key is only a placeholder; the proxy ignores it.

```ts
import { createOpenAICompatible } from '@ai-sdk/openai-compatible';

const gonka = createOpenAICompatible({
  name: 'gonka-local',
  baseURL: 'http://127.0.0.1:8080/v1',
  apiKey: 'local-placeholder',
});

export const model = gonka('gonka-virtual');
```

The `baseURL` intentionally ends in `/v1`. Do not configure individual upstream Provider URLs in OpenCode.

## Operational logs

Logs identify the numeric Provider declaration index and priority, Failover Failure category, HTTP status when available, Cooldown transitions, Recovery Wait transitions, and request cancellation. They never include request bodies, response bodies, authorization headers, or Provider API keys.

## Container smoke test

The smoke test builds the production image and a fake HTTPS OpenAI-compatible Provider, publishes the proxy on the host loopback interface, then sends a real request through that published endpoint:

```sh
sh scripts/smoke-test.sh
```

The fake Provider verifies the upstream bearer credential and the Model Alias before returning a completion-shaped response.
