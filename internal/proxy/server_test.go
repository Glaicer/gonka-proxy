package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
	"github.com/glaicer/gonka-proxy/internal/proxy"
)

func TestChatCompletionsLogsSafeOperationalEvents(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)

	primaryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","details":"upstream-response-secret"}}`)
	}))
	defer primaryProvider.Close()

	backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answer":"upstream-response-secret"}`)
	}))
	defer backupProvider.Close()

	handler, err := proxy.NewWithLogger(config.Config{
		ListenAddress:         "127.0.0.1:8080",
		Cooldown:              time.Minute,
		RecoveryWait:          time.Minute,
		ResponseHeaderTimeout: time.Second,
		Providers: []config.Provider{
			{Name: "primary", BaseURL: primaryProvider.URL + "/v1", APIKey: "primary-secret", ModelAlias: "primary-model", Priority: 100},
			{Name: "backup", BaseURL: backupProvider.URL + "/v1", APIKey: "backup-secret", ModelAlias: "backup-model", Priority: 50},
		},
	}, logger)
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	requestBody := `{"model":"virtual-model","messages":[{"role":"user","content":"do-not-log"}]}`
	response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}

	output := logs.String()
	for _, expected := range []string{
		"provider selected - primary - priority=100",
		"primary error - status=429 - rate limited",
		"primary - cooldown - 1m0s",
		"provider selected - backup - priority=50",
		"backup status=200",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("logs = %q, want %q", output, expected)
		}
	}
	for _, secret := range []string{"primary-secret", "backup-secret", "do-not-log", "upstream-response-secret"} {
		if strings.Contains(output, secret) {
			t.Errorf("logs contain sensitive value %q: %q", output, secret)
		}
	}
}

func TestChatCompletionsLogsRecoveryWaitCancellation(t *testing.T) {
	logs := &recoveryWaitLogWriter{started: make(chan struct{}), canceled: make(chan struct{})}
	logger := log.New(logs, "", 0)
	failoverFailure := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(failoverFailure)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer provider.Close()

	handler, err := proxy.NewWithLogger(config.Config{
		ListenAddress:         "127.0.0.1:8080",
		Cooldown:              time.Hour,
		RecoveryWait:          time.Hour,
		ResponseHeaderTimeout: time.Second,
		Providers: []config.Provider{
			{Name: "primary", BaseURL: provider.URL + "/v1", APIKey: "provider-secret", ModelAlias: "provider-model", Priority: 1},
		},
	}, logger)
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"virtual-model","messages":[]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	requestResult := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		requestResult <- requestErr
	}()

	select {
	case <-failoverFailure:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Failover Failure")
	}
	select {
	case <-logs.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Recovery Wait")
	}
	cancel()
	select {
	case <-requestResult:
	case <-time.After(time.Second):
		t.Fatal("canceled request did not finish")
	}
	select {
	case <-logs.canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Recovery Wait cancellation log")
	}

	output := logs.String()
	if !strings.Contains(output, "recovery wait - started - 1h0m0s") {
		t.Errorf("logs = %q, want Recovery Wait start", output)
	}
	if !strings.Contains(output, "recovery wait - canceled") {
		t.Errorf("logs = %q, want Recovery Wait cancellation", output)
	}
	if strings.Contains(output, "provider-secret") {
		t.Errorf("logs contain Provider API key: %q", output)
	}
}

type recoveryWaitLogWriter struct {
	mu           sync.Mutex
	buffer       bytes.Buffer
	started      chan struct{}
	canceled     chan struct{}
	startedOnce  sync.Once
	canceledOnce sync.Once
}

func (w *recoveryWaitLogWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	count, err := w.buffer.Write(value)
	if bytes.Contains(value, []byte("recovery wait - started")) {
		w.startedOnce.Do(func() {
			close(w.started)
		})
	}
	if bytes.Contains(value, []byte("recovery wait - canceled")) {
		w.canceledOnce.Do(func() {
			close(w.canceled)
		})
	}
	return count, err
}

func (w *recoveryWaitLogWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

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

func TestChatCompletionsFailsOverOnRateLimit(t *testing.T) {
	var rateLimitedHits atomic.Int32
	var backupHits atomic.Int32

	rateLimitedProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rateLimitedHits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer rateLimitedProvider.Close()

	backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"backup"}`)
	}))
	defer backupProvider.Close()

	server := newProxyServer(t, []providerFixture{
		{baseURL: rateLimitedProvider.URL + "/v1", apiKey: "rate-limited-secret", modelAlias: "rate-limited-model", priority: 100},
		{baseURL: backupProvider.URL + "/v1", apiKey: "backup-secret", modelAlias: "backup-model", priority: 50},
	})
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
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
	if string(responseBody) != `{"provider":"backup"}` {
		t.Fatalf("response body = %s, want backup response", responseBody)
	}
	if got := rateLimitedHits.Load(); got != 1 {
		t.Fatalf("rate-limited Provider received %d requests, want 1", got)
	}
	if got := backupHits.Load(); got != 1 {
		t.Fatalf("backup Provider received %d requests, want 1", got)
	}
}

func TestChatCompletionsFailsOverOnServerErrors(t *testing.T) {
	for _, statusCode := range []int{http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(fmt.Sprintf("status_%d", statusCode), func(t *testing.T) {
			var failedHits atomic.Int32
			var backupHits atomic.Int32

			failedProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				failedHits.Add(1)
				w.WriteHeader(statusCode)
			}))
			defer failedProvider.Close()

			backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backupHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"provider":"backup"}`)
			}))
			defer backupProvider.Close()

			server := newProxyServer(t, []providerFixture{
				{baseURL: failedProvider.URL + "/v1", apiKey: "failed-secret", modelAlias: "failed-model", priority: 100},
				{baseURL: backupProvider.URL + "/v1", apiKey: "backup-secret", modelAlias: "backup-model", priority: 50},
			})
			defer server.Close()

			resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
			if err != nil {
				t.Fatalf("proxy request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if got, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("read response body: %v", err)
			} else if string(got) != `{"provider":"backup"}` {
				t.Fatalf("response body = %s, want backup response", got)
			}
			if got := failedHits.Load(); got != 1 {
				t.Fatalf("failed Provider received %d requests, want 1", got)
			}
			if got := backupHits.Load(); got != 1 {
				t.Fatalf("backup Provider received %d requests, want 1", got)
			}
		})
	}
}

func TestChatCompletionsFailsOverOnNetworkFailure(t *testing.T) {
	var failedProviderHits atomic.Int32
	var backupProviderHits atomic.Int32

	failedProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failedProviderHits.Add(1)
	}))
	failedProviderURL := failedProvider.URL
	failedProvider.Close()

	backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupProviderHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"backup"}`)
	}))
	defer backupProvider.Close()

	server := newProxyServer(t, []providerFixture{
		{baseURL: failedProviderURL + "/v1", apiKey: "failed-secret", modelAlias: "failed-model", priority: 100},
		{baseURL: backupProvider.URL + "/v1", apiKey: "backup-secret", modelAlias: "backup-model", priority: 50},
	})
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read response body: %v", err)
	} else if string(got) != `{"provider":"backup"}` {
		t.Fatalf("response body = %s, want backup response", got)
	}
	if got := failedProviderHits.Load(); got != 0 {
		t.Fatalf("closed Provider received %d requests, want 0", got)
	}
	if got := backupProviderHits.Load(); got != 1 {
		t.Fatalf("backup Provider received %d requests, want 1", got)
	}
}

func TestChatCompletionsFailsOverOnResponseHeaderTimeout(t *testing.T) {
	var slowProviderHits atomic.Int32
	var backupProviderHits atomic.Int32
	releaseSlowProvider := make(chan struct{})

	slowProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowProviderHits.Add(1)
		select {
		case <-releaseSlowProvider:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(releaseSlowProvider)
		slowProvider.Close()
	}()

	backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupProviderHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"backup"}`)
	}))
	defer backupProvider.Close()

	server := newProxyServerWithTiming(t, []providerFixture{
		{baseURL: slowProvider.URL + "/v1", apiKey: "slow-secret", modelAlias: "slow-model", priority: 100},
		{baseURL: backupProvider.URL + "/v1", apiKey: "backup-secret", modelAlias: "backup-model", priority: 50},
	}, 2*time.Second, 20*time.Millisecond)
	defer server.Close()

	startedAt := time.Now()
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		t.Fatalf("request took %s, want response-header timeout failover", elapsed)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read response body: %v", err)
	} else if string(got) != `{"provider":"backup"}` {
		t.Fatalf("response body = %s, want backup response", got)
	}
	if got := slowProviderHits.Load(); got != 1 {
		t.Fatalf("slow Provider received %d requests, want 1", got)
	}
	if got := backupProviderHits.Load(); got != 1 {
		t.Fatalf("backup Provider received %d requests, want 1", got)
	}
}

func TestChatCompletionsFailsOverInPriorityOrderWithoutRepeatingAProvider(t *testing.T) {
	attempts := make(chan string, 3)

	lowProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts <- "low"
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"low"}`)
	}))
	defer lowProvider.Close()

	firstProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts <- "first"
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer firstProvider.Close()

	secondProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts <- "second"
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer secondProvider.Close()

	server := newProxyServer(t, []providerFixture{
		{baseURL: lowProvider.URL + "/v1", apiKey: "low-secret", modelAlias: "low-model", priority: 10},
		{baseURL: firstProvider.URL + "/v1", apiKey: "first-secret", modelAlias: "first-model", priority: 100},
		{baseURL: secondProvider.URL + "/v1", apiKey: "second-secret", modelAlias: "second-model", priority: 50},
	})
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
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
	if string(responseBody) != `{"provider":"low"}` {
		t.Fatalf("response body = %s, want low Provider response", responseBody)
	}

	var gotAttempts []string
	for range 3 {
		select {
		case attempt := <-attempts:
			gotAttempts = append(gotAttempts, attempt)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for Provider attempts")
		}
	}
	if want := []string{"first", "second", "low"}; !reflect.DeepEqual(gotAttempts, want) {
		t.Fatalf("Provider attempts = %v, want %v", gotAttempts, want)
	}
}

func TestChatCompletionsSkipsProviderDuringCooldownAndUsesItAfterExpiry(t *testing.T) {
	const cooldown = 40 * time.Millisecond
	var primaryHits atomic.Int32
	var backupHits atomic.Int32

	primaryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if primaryHits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"primary"}`)
	}))
	defer primaryProvider.Close()

	backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"backup"}`)
	}))
	defer backupProvider.Close()

	server := newProxyServerWithTiming(t, []providerFixture{
		{baseURL: primaryProvider.URL + "/v1", apiKey: "primary-secret", modelAlias: "primary-model", priority: 100},
		{baseURL: backupProvider.URL + "/v1", apiKey: "backup-secret", modelAlias: "backup-model", priority: 50},
	}, cooldown, time.Second)
	defer server.Close()

	post := func() (int, string) {
		t.Helper()
		resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
		if err != nil {
			t.Fatalf("proxy request: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read response body: %v", err)
		}
		return resp.StatusCode, string(body)
	}

	if status, body := post(); status != http.StatusOK || body != `{"provider":"backup"}` {
		t.Fatalf("first response = (%d, %s), want backup success", status, body)
	}
	if status, body := post(); status != http.StatusOK || body != `{"provider":"backup"}` {
		t.Fatalf("cooldown response = (%d, %s), want backup success", status, body)
	}
	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary Provider received %d requests during active cooldown, want 1", got)
	}

	deadline := time.Now().Add(time.Second)
	for primaryHits.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("primary Provider did not become available after cooldown expiry")
		}
		status, body := post()
		if status != http.StatusOK || body != `{"provider":"primary"}` {
			if time.Sleep(5 * time.Millisecond); time.Now().Before(deadline) {
				continue
			}
			t.Fatalf("post-expiry response = (%d, %s), want primary success", status, body)
		}
	}
	if got := primaryHits.Load(); got != 2 {
		t.Fatalf("primary Provider received %d requests after expiry, want exactly 2", got)
	}
	if got := backupHits.Load(); got < 2 {
		t.Fatalf("backup Provider received %d requests, want at least 2", got)
	}
}

func TestChatCompletionsSharesCooldownAcrossConcurrentRequests(t *testing.T) {
	var primaryHits atomic.Int32
	var backupHits atomic.Int32

	primaryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer primaryProvider.Close()

	backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"backup"}`)
	}))
	defer backupProvider.Close()

	server := newProxyServerWithTiming(t, []providerFixture{
		{baseURL: primaryProvider.URL + "/v1", apiKey: "primary-secret", modelAlias: "primary-model", priority: 100},
		{baseURL: backupProvider.URL + "/v1", apiKey: "backup-secret", modelAlias: "backup-model", priority: 50},
	}, time.Second, time.Second)
	defer server.Close()

	post := func() (int, string, error) {
		resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), err
	}

	if status, body, err := post(); err != nil || status != http.StatusOK || body != `{"provider":"backup"}` {
		t.Fatalf("initial response = (%d, %s, %v), want backup success", status, body, err)
	}

	const concurrentRequests = 8
	results := make(chan struct {
		status int
		body   string
		err    error
	}, concurrentRequests)
	var waitGroup sync.WaitGroup
	waitGroup.Add(concurrentRequests)
	for range concurrentRequests {
		go func() {
			defer waitGroup.Done()
			status, body, err := post()
			results <- struct {
				status int
				body   string
				err    error
			}{status: status, body: body, err: err}
		}()
	}
	waitGroup.Wait()
	close(results)

	for result := range results {
		if result.err != nil || result.status != http.StatusOK || result.body != `{"provider":"backup"}` {
			t.Fatalf("concurrent response = (%d, %s, %v), want backup success", result.status, result.body, result.err)
		}
	}
	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary Provider received %d requests, want 1 while cooldown is active", got)
	}
	if got := backupHits.Load(); got != concurrentRequests+1 {
		t.Fatalf("backup Provider received %d requests, want %d", got, concurrentRequests+1)
	}
}

func TestChatCompletionsWaitsForRecoveryBeforeNewRoutingPass(t *testing.T) {
	const recoveryWait = 40 * time.Millisecond
	var providerHits atomic.Int32

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if providerHits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"recovered"}`)
	}))
	defer provider.Close()

	server := newProxyServerWithRecoveryTiming(t, []providerFixture{
		{baseURL: provider.URL + "/v1", apiKey: "provider-secret", modelAlias: "provider-model", priority: 1},
	}, time.Second, recoveryWait, time.Second)
	defer server.Close()

	startedAt := time.Now()
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if elapsed := time.Since(startedAt); elapsed < recoveryWait*3/4 {
		t.Fatalf("request completed after %s, want Recovery Wait of about %s", elapsed, recoveryWait)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(body) != `{"provider":"recovered"}` {
		t.Fatalf("response body = %s, want recovered Provider response", body)
	}
	if got := providerHits.Load(); got != 2 {
		t.Fatalf("Provider received %d requests, want one attempt on each Routing Pass", got)
	}
}

func TestChatCompletionsStartsNewRoutingPassInPriorityOrder(t *testing.T) {
	const recoveryWait = 30 * time.Millisecond
	attempts := make(chan string, 4)
	var highHits atomic.Int32
	var lowHits atomic.Int32

	highProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := highHits.Add(1)
		attempts <- fmt.Sprintf("high-%d", attempt)
		if attempt < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"high"}`)
	}))
	defer highProvider.Close()

	lowProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := lowHits.Add(1)
		attempts <- fmt.Sprintf("low-%d", attempt)
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"low"}`)
	}))
	defer lowProvider.Close()

	server := newProxyServerWithRecoveryTiming(t, []providerFixture{
		{baseURL: lowProvider.URL + "/v1", apiKey: "low-secret", modelAlias: "low-model", priority: 10},
		{baseURL: highProvider.URL + "/v1", apiKey: "high-secret", modelAlias: "high-model", priority: 100},
	}, time.Second, recoveryWait, time.Second)
	defer server.Close()

	startedAt := time.Now()
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if elapsed := time.Since(startedAt); elapsed < recoveryWait*3/4 {
		t.Fatalf("request completed after %s, want Recovery Wait before the second Routing Pass", elapsed)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(body) != `{"provider":"low"}` {
		t.Fatalf("response body = %s, want low Provider response", body)
	}

	var gotAttempts []string
	for range 4 {
		select {
		case attempt := <-attempts:
			gotAttempts = append(gotAttempts, attempt)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for Provider attempts")
		}
	}
	if want := []string{"high-1", "low-1", "high-2", "low-2"}; !reflect.DeepEqual(gotAttempts, want) {
		t.Fatalf("Provider attempts = %v, want %v", gotAttempts, want)
	}
}

func TestChatCompletionsRepeatsRecoveryWaitAndRoutingPassUntilProviderSucceeds(t *testing.T) {
	const (
		failuresBeforeSuccess = 5
		recoveryWait          = 10 * time.Millisecond
	)
	var providerHits atomic.Int32

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if providerHits.Add(1) <= failuresBeforeSuccess {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"eventually-recovered"}`)
	}))
	defer provider.Close()

	server := newProxyServerWithRecoveryTiming(t, []providerFixture{
		{baseURL: provider.URL + "/v1", apiKey: "provider-secret", modelAlias: "provider-model", priority: 1},
	}, time.Second, recoveryWait, time.Second)
	defer server.Close()

	startedAt := time.Now()
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if elapsed := time.Since(startedAt); elapsed < recoveryWait*time.Duration(failuresBeforeSuccess)*3/4 {
		t.Fatalf("request completed after %s, want multiple Recovery Waits", elapsed)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(body) != `{"provider":"eventually-recovered"}` {
		t.Fatalf("response body = %s, want eventual recovery response", body)
	}
	if got := providerHits.Load(); got != failuresBeforeSuccess+1 {
		t.Fatalf("Provider received %d requests, want %d", got, failuresBeforeSuccess+1)
	}
}

func TestChatCompletionsCancelsRecoveryWait(t *testing.T) {
	const recoveryWait = 200 * time.Millisecond
	var providerHits atomic.Int32
	firstFailure := make(chan struct{})

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if providerHits.Add(1) == 1 {
			close(firstFailure)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		t.Error("Provider received a request after the downstream request was canceled")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer provider.Close()

	server := newProxyServerWithRecoveryTiming(t, []providerFixture{
		{baseURL: provider.URL + "/v1", apiKey: "provider-secret", modelAlias: "provider-model", priority: 1},
	}, time.Second, recoveryWait, time.Second)
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
	case <-firstFailure:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("timed out waiting for the first failed Provider attempt")
	}
	cancel()

	select {
	case requestErr := <-result:
		if requestErr == nil {
			t.Error("proxy request completed successfully after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("proxy request did not finish after Recovery Wait cancellation")
	}

	time.Sleep(recoveryWait / 2)
	if got := providerHits.Load(); got != 1 {
		t.Fatalf("Provider received %d requests after cancellation, want 1", got)
	}
}

func TestChatCompletionsReturnsClientErrorsWithoutFailoverOrCooldown(t *testing.T) {
	for _, testCase := range []struct {
		statusCode int
		body       string
	}{
		{statusCode: http.StatusBadRequest, body: `{"error":"bad request"}`},
		{statusCode: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
		{statusCode: http.StatusForbidden, body: `{"error":"forbidden"}`},
		{statusCode: http.StatusNotFound, body: `{"error":"not found"}`},
	} {
		t.Run(fmt.Sprintf("status_%d", testCase.statusCode), func(t *testing.T) {
			var primaryHits atomic.Int32
			var backupHits atomic.Int32

			primaryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Provider-Trace", "client-error")
				w.Header().Set("Connection", "keep-alive")
				w.WriteHeader(testCase.statusCode)
				_, _ = io.WriteString(w, testCase.body)
			}))
			defer primaryProvider.Close()

			backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backupHits.Add(1)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"provider":"backup"}`)
			}))
			defer backupProvider.Close()

			server := newProxyServer(t, []providerFixture{
				{baseURL: primaryProvider.URL + "/v1", apiKey: "primary-secret", modelAlias: "primary-model", priority: 100},
				{baseURL: backupProvider.URL + "/v1", apiKey: "backup-secret", modelAlias: "backup-model", priority: 50},
			})
			defer server.Close()

			for requestNumber := 0; requestNumber < 2; requestNumber++ {
				req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"virtual-model","messages":[],"stream":true}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("proxy request: %v", err)
				}
				responseBody, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Fatalf("read response body: %v", err)
				}
				if resp.StatusCode != testCase.statusCode {
					t.Fatalf("request %d status = %d, want %d", requestNumber+1, resp.StatusCode, testCase.statusCode)
				}
				if string(responseBody) != testCase.body {
					t.Fatalf("request %d body = %s, want %s", requestNumber+1, responseBody, testCase.body)
				}
				if got := resp.Header.Get("Content-Type"); got != "application/json" {
					t.Fatalf("request %d Content-Type = %q, want application/json", requestNumber+1, got)
				}
				if got := resp.Header.Get("X-Provider-Trace"); got != "client-error" {
					t.Fatalf("request %d X-Provider-Trace = %q, want client-error", requestNumber+1, got)
				}
				if got := resp.Header.Get("Connection"); got != "" {
					t.Fatalf("request %d Connection = %q, want hop-by-hop header excluded", requestNumber+1, got)
				}
			}

			if got := primaryHits.Load(); got != 2 {
				t.Fatalf("primary Provider received %d requests, want 2", got)
			}
			if got := backupHits.Load(); got != 0 {
				t.Fatalf("backup Provider received %d requests, want 0", got)
			}
		})
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

func TestChatCompletionsStreamsProviderResponsesIncrementally(t *testing.T) {
	firstChunkWritten := make(chan struct{})
	releaseProvider := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseProvider)
		})
	}

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Provider-Trace", "stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("streaming Provider does not support flushing")
			return
		}
		_, _ = io.WriteString(w, "data: {\"delta\":\"first\"}\n\n")
		flusher.Flush()
		close(firstChunkWritten)
		<-releaseProvider
		_, _ = io.WriteString(w, "data: {\"delta\":\"second\"}\n\ndata: [DONE]\n\n")
		flusher.Flush()
	}))
	defer func() {
		release()
		provider.Close()
	}()

	server := newProxyServer(t, []providerFixture{
		{baseURL: provider.URL + "/v1", apiKey: "provider-secret", modelAlias: "provider-model", priority: 1},
	})
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"virtual-model","messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	proxyResponse := make(chan struct {
		response *http.Response
		err      error
	}, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(req)
		proxyResponse <- struct {
			response *http.Response
			err      error
		}{response: response, err: requestErr}
	}()

	select {
	case <-firstChunkWritten:
	case <-time.After(time.Second):
		release()
		t.Fatal("timed out waiting for streaming Provider to write its first chunk")
	}

	var response *http.Response
	select {
	case result := <-proxyResponse:
		if result.err != nil {
			t.Fatalf("proxy request: %v", result.err)
		}
		response = result.response
	case <-time.After(time.Second):
		release()
		result := <-proxyResponse
		if result.response != nil {
			_ = result.response.Body.Close()
		}
		t.Fatalf("proxy did not expose streaming response headers promptly (request error: %v)", result.err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if got := response.Header.Get("X-Provider-Trace"); got != "stream" {
		t.Fatalf("X-Provider-Trace = %q, want stream", got)
	}

	firstRead := make(chan struct {
		body []byte
		err  error
	}, 1)
	go func() {
		buffer := make([]byte, 128)
		count, readErr := response.Body.Read(buffer)
		firstRead <- struct {
			body []byte
			err  error
		}{body: buffer[:count], err: readErr}
	}()

	var firstChunk []byte
	select {
	case result := <-firstRead:
		if result.err != nil && result.err != io.EOF {
			t.Fatalf("read first stream chunk: %v", result.err)
		}
		firstChunk = append([]byte(nil), result.body...)
	case <-time.After(time.Second):
		release()
		<-firstRead
		t.Fatal("proxy buffered the first Provider stream chunk")
	}

	const expectedFirstChunk = "data: {\"delta\":\"first\"}\n\n"
	if string(firstChunk) != expectedFirstChunk {
		t.Fatalf("first stream chunk = %q, want %q", firstChunk, expectedFirstChunk)
	}

	release()
	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read remaining stream: %v", err)
	}
	gotBody := append(firstChunk, rest...)
	wantBody := expectedFirstChunk + "data: {\"delta\":\"second\"}\n\ndata: [DONE]\n\n"
	if string(gotBody) != wantBody {
		t.Fatalf("stream body = %q, want %q", gotBody, wantBody)
	}
}

func TestChatCompletionsMarksInterruptedStreamCooldownWithoutFailover(t *testing.T) {
	var primaryHits atomic.Int32
	var backupHits atomic.Int32
	const interruptedChunk = "data: {\"provider\":\"primary\"}\n\n"

	primaryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("streaming Provider does not support flushing")
			return
		}
		_, _ = io.WriteString(w, interruptedChunk)
		flusher.Flush()
		// The declared length exceeds the bytes sent to simulate an interrupted generation.
	}))
	defer primaryProvider.Close()

	backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"backup"}`)
	}))
	defer backupProvider.Close()

	server := newProxyServerWithTiming(t, []providerFixture{
		{baseURL: primaryProvider.URL + "/v1", apiKey: "primary-secret", modelAlias: "primary-model", priority: 100},
		{baseURL: backupProvider.URL + "/v1", apiKey: "backup-secret", modelAlias: "backup-model", priority: 50},
	}, 200*time.Millisecond, time.Second)
	defer server.Close()

	firstResponse, err := http.Post(
		server.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"virtual-model","messages":[],"stream":true}`),
	)
	if err != nil {
		t.Fatalf("first proxy request: %v", err)
	}
	firstBody, err := io.ReadAll(firstResponse.Body)
	_ = firstResponse.Body.Close()
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read interrupted stream: %v", err)
	}
	if firstResponse.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want %d", firstResponse.StatusCode, http.StatusOK)
	}
	if string(firstBody) != interruptedChunk {
		t.Fatalf("first body = %q, want %q", firstBody, interruptedChunk)
	}
	if got := backupHits.Load(); got != 0 {
		t.Fatalf("backup Provider received %d requests during interrupted stream, want 0", got)
	}

	secondResponse, err := http.Post(
		server.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"virtual-model","messages":[]}`),
	)
	if err != nil {
		t.Fatalf("second proxy request: %v", err)
	}
	defer secondResponse.Body.Close()
	secondBody, err := io.ReadAll(secondResponse.Body)
	if err != nil {
		t.Fatalf("read second response: %v", err)
	}
	if secondResponse.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want %d", secondResponse.StatusCode, http.StatusOK)
	}
	if string(secondBody) != `{"provider":"backup"}` {
		t.Fatalf("second body = %s, want backup response", secondBody)
	}
	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary Provider received %d requests, want 1 while cooldown is active", got)
	}
	if got := backupHits.Load(); got != 1 {
		t.Fatalf("backup Provider received %d requests, want 1", got)
	}
}

func TestChatCompletionsCancelsStreamingProviderOnClientCancellation(t *testing.T) {
	streamStarted := make(chan struct{})
	streamCanceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("streaming Provider does not support flushing")
			return
		}
		_, _ = io.WriteString(w, "data: {\"delta\":\"active\"}\n\n")
		flusher.Flush()
		startedOnce.Do(func() {
			close(streamStarted)
		})
		<-r.Context().Done()
		canceledOnce.Do(func() {
			close(streamCanceled)
		})
	}))
	defer provider.Close()

	server := newProxyServer(t, []providerFixture{
		{baseURL: provider.URL + "/v1", apiKey: "provider-secret", modelAlias: "provider-model", priority: 1},
	})
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"virtual-model","messages":[],"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	proxyResponse := make(chan struct {
		response *http.Response
		err      error
	}, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(req)
		proxyResponse <- struct {
			response *http.Response
			err      error
		}{response: response, err: requestErr}
	}()

	select {
	case <-streamStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for streaming Provider request")
	}

	var response *http.Response
	select {
	case result := <-proxyResponse:
		if result.err != nil {
			t.Fatalf("proxy request: %v", result.err)
		}
		response = result.response
	case <-time.After(time.Second):
		t.Fatal("proxy did not expose streaming response headers")
	}
	defer response.Body.Close()

	firstChunkRead := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		buffer := make([]byte, 128)
		count, readErr := response.Body.Read(buffer)
		firstChunkRead <- struct {
			count int
			err   error
		}{count: count, err: readErr}
	}()
	select {
	case result := <-firstChunkRead:
		if result.count == 0 {
			t.Fatalf("first stream read returned no data (error: %v)", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first streamed chunk")
	}

	cancel()
	select {
	case <-streamCanceled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not close the active upstream stream")
	}
}

type providerFixture struct {
	name       string
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
	return newProxyServerWithTimingValues(t, providers, "", "")
}

func newProxyServerWithTiming(t *testing.T, providers []providerFixture, cooldown, responseHeaderTimeout time.Duration) *httptest.Server {
	t.Helper()
	return newProxyServerWithTimingValues(t, providers, cooldown.String(), responseHeaderTimeout.String())
}

func newProxyServerWithRecoveryTiming(t *testing.T, providers []providerFixture, cooldown, recoveryWait, responseHeaderTimeout time.Duration) *httptest.Server {
	t.Helper()
	return newProxyServerWithTimingValuesAndRecovery(t, providers, cooldown.String(), recoveryWait.String(), responseHeaderTimeout.String())
}

func newProxyServerWithTimingValues(t *testing.T, providers []providerFixture, cooldown, responseHeaderTimeout string) *httptest.Server {
	t.Helper()
	return newProxyServerWithTimingValuesAndRecovery(t, providers, cooldown, "", responseHeaderTimeout)
}

func newProxyServerWithTimingValuesAndRecovery(t *testing.T, providers []providerFixture, cooldown, recoveryWait, responseHeaderTimeout string) *httptest.Server {
	t.Helper()

	var yaml strings.Builder
	if cooldown != "" {
		fmt.Fprintf(&yaml, "cooldown: %s\n", cooldown)
	}
	if recoveryWait != "" {
		fmt.Fprintf(&yaml, "recovery_wait: %s\n", recoveryWait)
	}
	if responseHeaderTimeout != "" {
		fmt.Fprintf(&yaml, "response_header_timeout: %s\n", responseHeaderTimeout)
	}
	yaml.WriteString("providers:\n")
	for index, provider := range providers {
		name := provider.name
		if name == "" {
			name = fmt.Sprintf("provider-%d", index)
		}
		fmt.Fprintf(&yaml, "  - name: %q\n", name)
		fmt.Fprintf(&yaml, "    base_url: %q\n", provider.baseURL)
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
