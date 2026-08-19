# Gonka Proxy

Gonka Proxy exposes one local OpenAI-compatible endpoint and forwards chat completions to the highest-priority configured Provider.

## Run

Copy `config.example.yaml` to `config.yaml`, replace the Provider values, then run:

```sh
go run ./cmd/gonka-proxy --config config.yaml
```

The proxy listens on `0.0.0.0:8080` by default and accepts `POST /v1/chat/completions`.

Each Provider `base_url` is its OpenAI API root, normally ending in `/v1`. The proxy appends `/chat/completions`, replaces the incoming `model` with `model_alias`, and uses the Provider's `api_key` as its upstream bearer credential. Client authorization is ignored.

Providers are tried in descending `priority`; equal priorities retain YAML declaration order. Omitted timing values use the documented defaults in `config.example.yaml`.
