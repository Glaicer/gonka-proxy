# Proxy a happy-path request through the highest-priority Provider

Status: resolved

## Parent

[Gonka Proxy MVP PRD](../PRD.md)

## What to build

Create the first runnable Go service and its black-box HTTP test seam. Given a valid YAML configuration, the service accepts an OpenAI-compatible `POST /v1/chat/completions` request, selects the highest-priority Provider, replaces the Virtual Model with that Provider's Model Alias, replaces downstream authorization with the Provider API key, and transparently returns a successful non-streaming response.

This slice establishes deterministic ordering, configuration defaults, request cancellation propagation, and the public behavior that later failover and streaming slices extend.

User stories covered: 1–11, 26, 30–31.

## Acceptance criteria

- [x] A valid YAML configuration starts a Go HTTP service with the documented server and timing defaults.
- [x] `POST /v1/chat/completions` is exercised through a black-box test using local fake Providers.
- [x] The highest integer priority wins; equal priorities preserve YAML declaration order.
- [x] Only the selected Provider receives a successful happy-path request.
- [x] The upstream JSON contains the selected Provider's Model Alias instead of the incoming Virtual Model, while the remaining JSON is semantically preserved.
- [x] The upstream `Authorization` header contains only the selected Provider's bearer credential.
- [x] Missing, arbitrary, or placeholder downstream authorization is accepted and does not affect routing.
- [x] A successful non-streaming response preserves its client-relevant headers, status, and body while excluding hop-by-hop headers.
- [x] Downstream cancellation is propagated to the active upstream request.
- [x] Tests assert behavior only through the public HTTP seam and fake Provider observations.

## Blocked by

None - can start immediately.

## Comments

### 2026-08-19

Resolved in commit `7e6cf76`. Added the runnable Go service, YAML defaults and validation, deterministic Provider selection, request/response forwarding, cancellation propagation, and black-box HTTP coverage. `go test ./...`, `go vet ./...`, and `gofmt` pass.
