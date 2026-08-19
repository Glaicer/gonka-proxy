package proxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
	"github.com/glaicer/gonka-proxy/internal/proxy"
)

func TestChatCompletionsUsesHighestPriorityProvider(t *testing.T) {
	var lowHits atomic.Int32
	observations := make(chan upstreamObservation, 1)

	lowProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lowHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer lowProvider.Close()

	highProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			return
		}
		observations <- upstreamObservation{
			authorization: r.Header.Get("Authorization"),
			path:          r.URL.Path,
			body:          body,
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Provider-Trace", "highest")
		w.Header().Set("Connection", "keep-alive")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-highest","object":"chat.completion"}`)
	}))
	defer highProvider.Close()

	server := newProxyServer(t, []providerFixture{
		{baseURL: lowProvider.URL + "/v1", apiKey: "low-secret", modelAlias: "low-model", priority: 10},
		{baseURL: highProvider.URL + "/v1", apiKey: "high-secret", modelAlias: "high-model", priority: 100},
	})
	defer server.Close()

	requestBody := `{"model":"virtual-model","messages":[{"role":"user","content":"hello"}],"stream":false,"temperature":0.2,"metadata":{"request_id":"abc"}}`
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer arbitrary-placeholder")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(responseBody) != `{"id":"chatcmpl-highest","object":"chat.completion"}` {
		t.Fatalf("response body = %s", responseBody)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := resp.Header.Get("X-Provider-Trace"); got != "highest" {
		t.Fatalf("X-Provider-Trace = %q, want highest", got)
	}
	if got := resp.Header.Get("Connection"); got != "" {
		t.Fatalf("Connection = %q, want hop-by-hop header excluded", got)
	}

	select {
	case observation := <-observations:
		if observation.authorization != "Bearer high-secret" {
			t.Errorf("upstream Authorization = %q, want selected Provider credential", observation.authorization)
		}
		if observation.path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", observation.path)
		}
		assertRequestBody(t, observation.body, requestBody, "high-model")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream observation")
	}
	if got := lowHits.Load(); got != 0 {
		t.Fatalf("lower-priority Provider received %d requests, want 0", got)
	}
}

func TestChatCompletionsPreservesDeclarationOrderForEqualPriority(t *testing.T) {
	var firstHits atomic.Int32
	var secondHits atomic.Int32

	firstProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"first"}`)
	}))
	defer firstProvider.Close()

	secondProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"second"}`)
	}))
	defer secondProvider.Close()

	server := newProxyServer(t, []providerFixture{
		{baseURL: firstProvider.URL + "/v1", apiKey: "first-secret", modelAlias: "first-model", priority: 50},
		{baseURL: secondProvider.URL + "/v1", apiKey: "second-secret", modelAlias: "second-model", priority: 50},
	})
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(responseBody) != `{"provider":"first"}` {
		t.Fatalf("response body = %s, want first Provider response", responseBody)
	}
	if got := firstHits.Load(); got != 1 {
		t.Fatalf("first Provider received %d requests, want 1", got)
	}
	if got := secondHits.Load(); got != 0 {
		t.Fatalf("second Provider received %d requests, want 0", got)
	}
}

func TestChatCompletionsIgnoresDownstreamAuthorization(t *testing.T) {
	observedAuthorization := make(chan string, 2)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedAuthorization <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer provider.Close()

	server := newProxyServer(t, []providerFixture{
		{baseURL: provider.URL + "/v1", apiKey: "provider-secret", modelAlias: "provider-model", priority: 1},
	})
	defer server.Close()

	for _, authorization := range []string{"", "Bearer arbitrary-placeholder"} {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy request with %q: %v", authorization, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status with %q = %d, want %d", authorization, resp.StatusCode, http.StatusOK)
		}
	}

	for range 2 {
		select {
		case got := <-observedAuthorization:
			if got != "Bearer provider-secret" {
				t.Errorf("upstream Authorization = %q, want provider credential", got)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for upstream authorization")
		}
	}
}

func TestChatCompletionsPropagatesDownstreamCancellation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startedOnce atomic.Bool
	var canceledOnce atomic.Bool

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if startedOnce.CompareAndSwap(false, true) {
			close(started)
		}
		_, _ = io.ReadAll(r.Body)
		<-r.Context().Done()
		if canceledOnce.CompareAndSwap(false, true) {
			close(canceled)
		}
	}))
	defer provider.Close()

	server := newProxyServer(t, []providerFixture{
		{baseURL: provider.URL + "/v1", apiKey: "provider-secret", modelAlias: "provider-model", priority: 1},
	})
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		resp, requestErr := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		result <- requestErr
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("timed out waiting for upstream request")
	}
	cancel()

	select {
	case requestErr := <-result:
		if requestErr == nil {
			t.Error("proxy request completed successfully after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("proxy request did not finish after downstream cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not observe downstream cancellation")
	}
}

type providerFixture struct {
	baseURL    string
	apiKey     string
	modelAlias string
	priority   int
}

type upstreamObservation struct {
	authorization string
	path          string
	body          []byte
}

func newProxyServer(t *testing.T, providers []providerFixture) *httptest.Server {
	t.Helper()

	var yaml strings.Builder
	yaml.WriteString("providers:\n")
	for _, provider := range providers {
		fmt.Fprintf(&yaml, "  - base_url: %q\n", provider.baseURL)
		fmt.Fprintf(&yaml, "    api_key: %q\n", provider.apiKey)
		fmt.Fprintf(&yaml, "    model_alias: %q\n", provider.modelAlias)
		fmt.Fprintf(&yaml, "    priority: %d\n", provider.priority)
	}

	configPath := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(configPath, []byte(yaml.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	handler, err := proxy.New(cfg)
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	return httptest.NewServer(handler)
}

func assertRequestBody(t *testing.T, gotBody []byte, wantBody string, wantModelAlias string) {
	t.Helper()

	var got map[string]any
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("decode upstream body %q: %v", gotBody, err)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(wantBody), &want); err != nil {
		t.Fatalf("decode expected body: %v", err)
	}
	if got["model"] != wantModelAlias {
		t.Errorf("upstream Model Alias = %v, want %q", got["model"], wantModelAlias)
	}
	delete(got, "model")
	delete(want, "model")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upstream body fields = %#v, want %#v", got, want)
	}
}
