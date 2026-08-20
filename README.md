# Gonka Proxy

Gonka Proxy exposes one local OpenAI-compatible endpoint and forwards chat completions to the highest-priority configured Provider.

## Quick start

```sh
cp config.example.yaml config.yaml
# Replace the example Provider names, URLs, keys, and model aliases.
docker compose up --build
```

Docker publishes the service only on `127.0.0.1:8080`. The YAML file is mounted read-only, loaded once at startup, and must be replaced followed by a container restart to apply changes.

The complete Docker, operations, OpenCode, and smoke-test workflow is in [`docs/docker-and-opencode.md`](docs/docker-and-opencode.md).

For a native run:

```sh
go run ./cmd/gonka-proxy --config config.yaml
```
