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

	reasoningMax := config.ReasoningEffortMax
	handler, err := proxy.NewWithLogger(config.Config{
		ListenAddress:         "127.0.0.1:8080",
		Cooldown:              time.Minute,
		RecoveryWait:          time.Minute,
		ResponseHeaderTimeout: time.Second,
		LogLevel:              config.LogLevelInfo,
		ReasoningEffort:       &reasoningMax,
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

func TestChatCompletionsAlwaysLogsSuccessfulStatus(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answer":"ok"}`)
	}))
	defer provider.Close()

	server, logs := newLoggedProxyServer(t, []providerFixture{
		{baseURL: provider.URL + "/v1", apiKey: "provider-secret", modelAlias: "provider-model", priority: 1},
	}, "")
	defer server.Close()

	response, err := http.Post(
		server.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"virtual-model","messages":[]}`),
	)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = response.Body.Close()

	if output := logs.String(); !strings.Contains(output, "provider-0 status=200") {
		t.Fatalf("logs = %q, want successful status log", output)
	}
}

func TestChatCompletionsPayloadLogsOnlyAtInfo(t *testing.T) {
	for _, logLevel := range []config.LogLevel{
		config.LogLevelInfo,
		config.LogLevelWarn,
		config.LogLevelError,
		"",
	} {
		t.Run(string(logLevel), func(t *testing.T) {
			t.Run("provider error body", func(t *testing.T) {
				const errorMessage = "provider-error-payload-diagnostic"
				const primaryAPIKey = "primary-secret"
				const backupAPIKey = "backup-secret"
				primaryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"message":"`+errorMessage+` reflected Bearer `+primaryAPIKey+` and `+backupAPIKey+`"}}`)
				}))
				defer primaryProvider.Close()

				backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"provider":"backup"}`)
				}))
				defer backupProvider.Close()

				server, logs := newLoggedProxyServer(t, []providerFixture{
					{baseURL: primaryProvider.URL + "/v1", apiKey: primaryAPIKey, modelAlias: "primary-model", priority: 100},
					{baseURL: backupProvider.URL + "/v1", apiKey: backupAPIKey, modelAlias: "backup-model", priority: 50},
				}, logLevel)
				defer server.Close()

				response, err := http.Post(
					server.URL+"/v1/chat/completions",
					"application/json",
					strings.NewReader(`{"model":"virtual-model","messages":[]}`),
				)
				if err != nil {
					t.Fatalf("proxy request: %v", err)
				}
				if _, err := io.ReadAll(response.Body); err != nil {
					t.Fatalf("read response: %v", err)
				}
				_ = response.Body.Close()

				output := logs.String()
				gotPayload := strings.Contains(output, errorMessage)
				wantPayload := logLevel == config.LogLevelInfo
				if gotPayload != wantPayload {
					t.Fatalf("logs = %q, payload present = %t, want %t", output, gotPayload, wantPayload)
				}
				for _, secret := range []string{primaryAPIKey, backupAPIKey, "Bearer " + primaryAPIKey} {
					if strings.Contains(output, secret) {
						t.Fatalf("logs contain reflected Provider secret %q: %q", secret, output)
					}
				}
				successLogged := strings.Contains(output, "provider-1 status=200")
				if !successLogged {
					t.Fatalf("success status log present = %t, want true at %s", successLogged, logLevel)
				}
			})

			t.Run("stream tail", func(t *testing.T) {
				const streamTail = "stream-tail-payload-diagnostic"
				const primaryAPIKey = "provider-secret"
				const backupAPIKey = "backup-secret"
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "data: {\"detail\":\"reflected Bearer "+primaryAPIKey+" and "+backupAPIKey+" "+streamTail+"\"}\n\n")
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
				}))
				defer provider.Close()
				backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"provider":"backup"}`)
				}))
				defer backupProvider.Close()

				server, logs := newLoggedProxyServer(t, []providerFixture{
					{baseURL: provider.URL + "/v1", apiKey: primaryAPIKey, modelAlias: "provider-model", priority: 100},
					{baseURL: backupProvider.URL + "/v1", apiKey: backupAPIKey, modelAlias: "backup-model", priority: 50},
				}, logLevel)
				defer server.Close()

				response, err := http.Post(
					server.URL+"/v1/chat/completions",
					"application/json",
					strings.NewReader(`{"model":"virtual-model","messages":[],"stream":true}`),
				)
				if err != nil {
					t.Fatalf("proxy request: %v", err)
				}
				if _, err := io.ReadAll(response.Body); err != nil {
					t.Fatalf("read stream: %v", err)
				}
				_ = response.Body.Close()

				output := logs.String()
				gotPayload := strings.Contains(output, streamTail)
				wantPayload := logLevel == config.LogLevelInfo
				if gotPayload != wantPayload {
					t.Fatalf("logs = %q, stream tail present = %t, want %t", output, gotPayload, wantPayload)
				}
				for _, secret := range []string{primaryAPIKey, backupAPIKey, "Bearer " + primaryAPIKey} {
					if strings.Contains(output, secret) {
						t.Fatalf("logs contain reflected Provider secret %q: %q", secret, output)
					}
				}
			})
		})
	}
}

func TestChatCompletionsRedactsProviderNamesAtInfoAndDefaultWarn(t *testing.T) {
	for _, logLevel := range []config.LogLevel{config.LogLevelInfo, ""} {
		t.Run(string(logLevel), func(t *testing.T) {
			const primaryAPIKey = "primary-name-secret"
			const backupAPIKey = "backup-name-secret"
			primaryName := "primary-" + primaryAPIKey + "-and-" + backupAPIKey
			backupName := "backup-" + backupAPIKey + "-and-" + primaryAPIKey

			primaryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer primaryProvider.Close()
			backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"provider":"backup"}`)
			}))
			defer backupProvider.Close()

			server, logs := newLoggedProxyServer(t, []providerFixture{
				{name: primaryName, baseURL: primaryProvider.URL + "/v1", apiKey: primaryAPIKey, modelAlias: "primary-model", priority: 100},
				{name: backupName, baseURL: backupProvider.URL + "/v1", apiKey: backupAPIKey, modelAlias: "backup-model", priority: 50},
			}, logLevel)
			defer server.Close()

			response, err := http.Post(
				server.URL+"/v1/chat/completions",
				"application/json",
				strings.NewReader(`{"model":"virtual-model","messages":[]}`),
			)
			if err != nil {
				t.Fatalf("proxy request: %v", err)
			}
			if _, err := io.ReadAll(response.Body); err != nil {
				t.Fatalf("read response: %v", err)
			}
			_ = response.Body.Close()

			output := logs.String()
			for _, secret := range []string{primaryAPIKey, backupAPIKey} {
				if strings.Contains(output, secret) {
					t.Fatalf("logs contain Provider-name secret %q: %q", secret, output)
				}
			}
			if !strings.Contains(output, "[REDACTED]") {
				t.Fatalf("logs = %q, want redacted Provider-name diagnostics", output)
			}
		})
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
		LogLevel:              config.LogLevelInfo,
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
		assertRequestBody(t, observation.body, requestBody, "high-model", stringPtr("max"))
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

func TestChatCompletionsFailsOverOnPaymentRequired(t *testing.T) {
	var paymentRequiredHits atomic.Int32
	var backupHits atomic.Int32

	paymentRequiredProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paymentRequiredHits.Add(1)
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":"insufficient credits"}`)
	}))
	defer paymentRequiredProvider.Close()

	backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider":"backup"}`)
	}))
	defer backupProvider.Close()

	server := newProxyServer(t, []providerFixture{
		{baseURL: paymentRequiredProvider.URL + "/v1", apiKey: "payment-required-secret", modelAlias: "payment-required-model", priority: 100},
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
	if got := paymentRequiredHits.Load(); got != 1 {
		t.Fatalf("payment-required Provider received %d requests, want 1", got)
	}
	if got := backupHits.Load(); got != 1 {
		t.Fatalf("backup Provider received %d requests, want 1", got)
	}
}

func TestChatCompletionsFailsOverOnUnsupportedReasoningEffort(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
	}{
		{name: "nested_error_message", body: `{"error":{"message":"reasoning_effort: unsupported value: max","type":"invalid_request_error"}}`},
		{name: "top_level_message", body: `{"message":"reasoning_effort: unsupported value: xhigh"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var primaryHits atomic.Int32
			var backupHits atomic.Int32

			primaryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, testCase.body)
			}))
			defer primaryProvider.Close()

			backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backupHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"provider":"backup"}`)
			}))
			defer backupProvider.Close()

			server := newProxyServer(t, []providerFixture{
				{baseURL: primaryProvider.URL + "/v1", apiKey: "primary-secret", modelAlias: "primary-model", priority: 100},
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
			if got := primaryHits.Load(); got != 1 {
				t.Fatalf("primary Provider received %d requests, want 1", got)
			}
			if got := backupHits.Load(); got != 1 {
				t.Fatalf("backup Provider received %d requests, want 1", got)
			}
		})
	}
}

func TestChatCompletionsReturnsOtherClientErrorsWithoutFailover(t *testing.T) {
	for _, testCase := range []struct {
		statusCode int
		body       string
	}{
		{statusCode: http.StatusBadRequest, body: `{"error":{"message":"invalid messages payload"}}`},
		{statusCode: http.StatusBadRequest, body: `{"error":"reasoning_effort must be a string"}`},
		{statusCode: http.StatusBadRequest, body: `{"message":"reasoning_effort: unsupported thing"}`},
		{statusCode: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
	} {
		t.Run(fmt.Sprintf("status_%d", testCase.statusCode), func(t *testing.T) {
			var primaryHits atomic.Int32
			var backupHits atomic.Int32

			primaryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryHits.Add(1)
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

			resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
			if err != nil {
				t.Fatalf("proxy request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != testCase.statusCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, testCase.statusCode)
			}
			if got := primaryHits.Load(); got != 1 {
				t.Fatalf("primary Provider received %d requests, want 1", got)
			}
			if got := backupHits.Load(); got != 0 {
				t.Fatalf("backup Provider received %d requests, want 0", got)
			}
		})
	}
}

func TestChatCompletionsFailsOverWithoutReadingStalledFailoverBody(t *testing.T) {
	for _, statusCode := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusPaymentRequired} {
		t.Run(fmt.Sprintf("status_%d", statusCode), func(t *testing.T) {
			var failedHits atomic.Int32
			var backupHits atomic.Int32
			releaseFailedProvider := make(chan struct{})
			var releaseOnce sync.Once

			failedProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				failedHits.Add(1)
				w.WriteHeader(statusCode)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				// Headers are available, but the body never reaches EOF until the test
				// releases the handler. The proxy must fail over without waiting for it.
				select {
				case <-releaseFailedProvider:
				case <-r.Context().Done():
				}
			}))
			defer func() {
				releaseOnce.Do(func() { close(releaseFailedProvider) })
				failedProvider.Close()
			}()

			backupProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backupHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"provider":"backup"}`)
			}))
			defer backupProvider.Close()

			server := newProxyServerWithTiming(t, []providerFixture{
				{baseURL: failedProvider.URL + "/v1", apiKey: "failed-secret", modelAlias: "failed-model", priority: 100},
				{baseURL: backupProvider.URL + "/v1", apiKey: "backup-secret", modelAlias: "backup-model", priority: 50},
			}, time.Second, time.Second)
			defer server.Close()

			startedAt := time.Now()
			client := &http.Client{Timeout: 500 * time.Millisecond}
			response, err := client.Post(
				server.URL+"/v1/chat/completions",
				"application/json",
				strings.NewReader(`{"model":"virtual-model","messages":[]}`),
			)
			if err != nil {
				t.Fatalf("proxy request: %v", err)
			}
			defer response.Body.Close()
			if elapsed := time.Since(startedAt); elapsed >= 400*time.Millisecond {
				t.Fatalf("request took %s, want failover without waiting for body", elapsed)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read response body: %v", err)
			}
			if string(body) != `{"provider":"backup"}` {
				t.Fatalf("response body = %s, want backup response", body)
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

func TestChatCompletionsMarksNormalEOFWithoutDoneCooldownWithoutFailover(t *testing.T) {
	var primaryHits atomic.Int32
	var backupHits atomic.Int32
	const interruptedChunk = "data: {\"provider\":\"primary\",\"content\":\"[DONE]\"}\n\n"

	primaryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("streaming Provider does not support flushing")
			return
		}
		_, _ = io.WriteString(w, interruptedChunk)
		flusher.Flush()
		// Returning normally produces a clean EOF without the required [DONE] marker.
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
	if err != nil {
		t.Fatalf("read first stream: %v", err)
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

func TestChatCompletionsRedactsProviderKeyAcrossStreamTailBoundary(t *testing.T) {
	const apiKey = "provider-key-SECRET-0123456789"
	const secretSuffix = "SECRET-0123456789"
	const usefulTail = "nonsecret-tail-marker"
	streamBody := "data: " + strings.Repeat("x", 60) + apiKey + strings.Repeat("y", 20) + " " + usefulTail + "\n\n"

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, streamBody)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer provider.Close()

	server, logs := newLoggedProxyServer(t, []providerFixture{
		{baseURL: provider.URL + "/v1", apiKey: apiKey, modelAlias: "provider-model", priority: 1},
	}, config.LogLevelInfo)
	defer server.Close()

	response, err := http.Post(
		server.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"virtual-model","messages":[],"stream":true}`),
	)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	_ = response.Body.Close()

	output := logs.String()
	for _, secret := range []string{apiKey, secretSuffix} {
		if strings.Contains(output, secret) {
			t.Fatalf("logs contain full or partial Provider key %q: %q", secret, output)
		}
	}
	if !strings.Contains(output, usefulTail) {
		t.Fatalf("logs = %q, want useful nonsecret stream tail", output)
	}
}

func TestChatCompletionsDoesNotTreatLongDataLineSuffixAsDone(t *testing.T) {
	var primaryHits atomic.Int32
	var backupHits atomic.Int32
	const doneLikeSuffix = "data:[DONE]"
	longDataLine := "data: {\"payload\":\"" + strings.Repeat("x", 100) + doneLikeSuffix + strings.Repeat("y", 60) + "\"}\n\n"

	primaryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, longDataLine)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
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

	firstResponse, err := http.Post(
		server.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"virtual-model","messages":[],"stream":true}`),
	)
	if err != nil {
		t.Fatalf("first proxy request: %v", err)
	}
	if _, err := io.ReadAll(firstResponse.Body); err != nil {
		t.Fatalf("read first stream: %v", err)
	}
	_ = firstResponse.Body.Close()

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
	if string(secondBody) != `{"provider":"backup"}` {
		t.Fatalf("second body = %s, want backup response", secondBody)
	}
	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary Provider received %d requests, want 1 after long data line", got)
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

func TestChatCompletionsStripsReasoningEffortWhenDisabled(t *testing.T) {
	observation := make(chan upstreamObservation, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			return
		}
		observation <- upstreamObservation{body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer provider.Close()

	handler, err := proxy.NewWithLogger(config.Config{
		ListenAddress:         "127.0.0.1:8080",
		Cooldown:              time.Second,
		RecoveryWait:          time.Second,
		ResponseHeaderTimeout: time.Second,
		ReasoningEffort:       nil,
		Providers: []config.Provider{
			{Name: "primary", BaseURL: provider.URL + "/v1", APIKey: "provider-secret", ModelAlias: "provider-model", Priority: 10},
		},
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	// Client sends reasoning_effort=high, but config is disabled (nil) so upstream must be stripped.
	requestBody := `{"model":"virtual-model","reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	_, _ = io.ReadAll(resp.Body)

	select {
	case obs := <-observation:
		assertRequestBody(t, obs.body, requestBody, "provider-model", nil)
		// also ensure stripping works when client omits reasoning_effort
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(obs.body, &payload); err != nil {
			t.Fatalf("decode upstream payload: %v", err)
		}
		if _, ok := payload["reasoning_effort"]; ok {
			t.Errorf("upstream should not contain reasoning_effort when disabled, got %s", string(obs.body))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream observation")
	}

	// Second request without reasoning_effort from client should also remain absent.
	observation2 := make(chan upstreamObservation, 1)
	provider2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		observation2 <- upstreamObservation{body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer provider2.Close()
	handler2, err := proxy.NewWithLogger(config.Config{
		ListenAddress:         "127.0.0.1:8080",
		Cooldown:              time.Second,
		RecoveryWait:          time.Second,
		ResponseHeaderTimeout: time.Second,
		ReasoningEffort:       nil,
		Providers: []config.Provider{
			{Name: "primary", BaseURL: provider2.URL + "/v1", APIKey: "provider-secret", ModelAlias: "provider-model", Priority: 10},
		},
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	server2 := httptest.NewServer(handler2)
	defer server2.Close()
	requestBody2 := `{"model":"virtual-model","messages":[]}`
	resp2, err := http.Post(server2.URL+"/v1/chat/completions", "application/json", strings.NewReader(requestBody2))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp2.Body.Close()
	_, _ = io.ReadAll(resp2.Body)
	select {
	case obs := <-observation2:
		assertRequestBody(t, obs.body, requestBody2, "provider-model", nil)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second upstream observation")
	}
}

func TestChatCompletionsOverwritesReasoningEffort(t *testing.T) {
	tests := []struct {
		name         string
		configEffort config.ReasoningEffort
		clientBody   string
		wantEffort   string
	}{
		{
			name:         "low overwrites high",
			configEffort: config.ReasoningEffortLow,
			clientBody:   `{"model":"virtual-model","reasoning_effort":"high","messages":[]}`,
			wantEffort:   "low",
		},
		{
			name:         "max overwrites low",
			configEffort: config.ReasoningEffortMax,
			clientBody:   `{"model":"virtual-model","reasoning_effort":"low","messages":[]}`,
			wantEffort:   "max",
		},
		{
			name:         "high sets when absent",
			configEffort: config.ReasoningEffortHigh,
			clientBody:   `{"model":"virtual-model","messages":[]}`,
			wantEffort:   "high",
		},
		{
			name:         "low normalizes high input",
			configEffort: config.ReasoningEffortLow,
			clientBody:   `{"model":"virtual-model","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`,
			wantEffort:   "low",
		},
		{
			name:         "none forwards when absent",
			configEffort: config.ReasoningEffortNone,
			clientBody:   `{"model":"virtual-model","messages":[]}`,
			wantEffort:   "none",
		},
		{
			name:         "medium overwrites client high",
			configEffort: config.ReasoningEffortMedium,
			clientBody:   `{"model":"virtual-model","reasoning_effort":"high","messages":[]}`,
			wantEffort:   "medium",
		},
		{
			name:         "xhigh overwrites client low",
			configEffort: config.ReasoningEffortXHigh,
			clientBody:   `{"model":"virtual-model","reasoning_effort":"low","messages":[]}`,
			wantEffort:   "xhigh",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			observation := make(chan upstreamObservation, 1)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream body: %v", err)
					return
				}
				observation <- upstreamObservation{body: body}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer provider.Close()

			effort := tc.configEffort
			handler, err := proxy.NewWithLogger(config.Config{
				ListenAddress:         "127.0.0.1:8080",
				Cooldown:              time.Second,
				RecoveryWait:          time.Second,
				ResponseHeaderTimeout: time.Second,
				ReasoningEffort:       &effort,
				Providers: []config.Provider{
					{Name: "primary", BaseURL: provider.URL + "/v1", APIKey: "provider-secret", ModelAlias: "provider-model", Priority: 10},
				},
			}, log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatalf("create proxy: %v", err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()

			resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(tc.clientBody))
			if err != nil {
				t.Fatalf("proxy request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			_, _ = io.ReadAll(resp.Body)

			select {
			case obs := <-observation:
				assertRequestBody(t, obs.body, tc.clientBody, "provider-model", stringPtr(tc.wantEffort))
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for upstream observation")
			}
		})
	}
}

func TestChatCompletionsAppliesPerProviderReasoningEffort(t *testing.T) {
	tests := []struct {
		name           string
		globalEffort   *config.ReasoningEffort
		providerEffort *config.ReasoningEffort
		providerStrip  bool
		clientBody     string
		wantEffort     *string
	}{
		{
			name:           "override wins over global",
			globalEffort:   effortPtr(config.ReasoningEffortMax),
			providerEffort: effortPtr(config.ReasoningEffortHigh),
			clientBody:     `{"model":"virtual-model","reasoning_effort":"low","messages":[]}`,
			wantEffort:     stringPtr("high"),
		},
		{
			name:          "strip overrides global even when client supplies one",
			globalEffort:  effortPtr(config.ReasoningEffortMax),
			providerStrip: true,
			clientBody:    `{"model":"virtual-model","reasoning_effort":"low","messages":[]}`,
			wantEffort:    nil,
		},
		{
			name:         "absent inherits global",
			globalEffort: effortPtr(config.ReasoningEffortMax),
			clientBody:   `{"model":"virtual-model","reasoning_effort":"low","messages":[]}`,
			wantEffort:   stringPtr("max"),
		},
		{
			name:          "strip with null global stays stripped",
			globalEffort:  nil,
			providerStrip: true,
			clientBody:    `{"model":"virtual-model","reasoning_effort":"low","messages":[]}`,
			wantEffort:    nil,
		},
		{
			name:           "override normalizes case",
			globalEffort:   nil,
			providerEffort: effortPtr(config.ReasoningEffort("HIGH")),
			clientBody:     `{"model":"virtual-model","messages":[]}`,
			wantEffort:     stringPtr("high"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			observation := make(chan upstreamObservation, 1)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream body: %v", err)
					return
				}
				observation <- upstreamObservation{body: body}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer provider.Close()

			handler, err := proxy.NewWithLogger(config.Config{
				ListenAddress:         "127.0.0.1:8080",
				Cooldown:              time.Second,
				RecoveryWait:          time.Second,
				ResponseHeaderTimeout: time.Second,
				ReasoningEffort:       tc.globalEffort,
				Providers: []config.Provider{
					{Name: "primary", BaseURL: provider.URL + "/v1", APIKey: "provider-secret", ModelAlias: "provider-model", Priority: 10, ReasoningEffort: tc.providerEffort, StripReasoningEffort: tc.providerStrip},
				},
			}, log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatalf("create proxy: %v", err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()

			resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(tc.clientBody))
			if err != nil {
				t.Fatalf("proxy request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			_, _ = io.ReadAll(resp.Body)

			select {
			case obs := <-observation:
				assertRequestBody(t, obs.body, tc.clientBody, "provider-model", tc.wantEffort)
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for upstream observation")
			}
		})
	}
}

func TestChatCompletionsFailoverUsesEachProvidersReasoningEffort(t *testing.T) {
	observations := make(map[string]chan upstreamObservation, 2)
	observations["primary"] = make(chan upstreamObservation, 1)
	observations["backup"] = make(chan upstreamObservation, 1)

	newObservedProvider := func(name string, statusCode int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upstream body: %v", err)
				return
			}
			observations[name] <- upstreamObservation{body: body}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(statusCode)
			_, _ = io.WriteString(w, `{"ok":true}`)
		}))
	}
	failing := newObservedProvider("primary", http.StatusInternalServerError)
	defer failing.Close()
	healthy := newObservedProvider("backup", http.StatusOK)
	defer healthy.Close()

	handler, err := proxy.NewWithLogger(config.Config{
		ListenAddress:         "127.0.0.1:8080",
		Cooldown:              time.Second,
		RecoveryWait:          time.Second,
		ResponseHeaderTimeout: time.Second,
		ReasoningEffort:       effortPtr(config.ReasoningEffortLow),
		Providers: []config.Provider{
			{Name: "primary", BaseURL: failing.URL + "/v1", APIKey: "primary-secret", ModelAlias: "primary-model", Priority: 100, StripReasoningEffort: true},
			{Name: "backup", BaseURL: healthy.URL + "/v1", APIKey: "backup-secret", ModelAlias: "backup-model", Priority: 50, ReasoningEffort: effortPtr(config.ReasoningEffortHigh)},
		},
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	requestBody := `{"model":"virtual-model","reasoning_effort":"low","messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	_, _ = io.ReadAll(resp.Body)

	select {
	case obs := <-observations["primary"]:
		assertRequestBody(t, obs.body, requestBody, "primary-model", nil)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for primary upstream observation")
	}
	select {
	case obs := <-observations["backup"]:
		assertRequestBody(t, obs.body, requestBody, "backup-model", stringPtr("high"))
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backup upstream observation")
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

func newLoggedProxyServer(t *testing.T, providers []providerFixture, logLevel config.LogLevel) (*httptest.Server, *bytes.Buffer) {
	t.Helper()

	reasoningMax := config.ReasoningEffortMax
	configuredProviders := make([]config.Provider, 0, len(providers))
	for index, provider := range providers {
		name := provider.name
		if name == "" {
			name = fmt.Sprintf("provider-%d", index)
		}
		configuredProviders = append(configuredProviders, config.Provider{
			Name:       name,
			BaseURL:    provider.baseURL,
			APIKey:     provider.apiKey,
			ModelAlias: provider.modelAlias,
			Priority:   provider.priority,
		})
	}

	var logs bytes.Buffer
	handler, err := proxy.NewWithLogger(config.Config{
		ListenAddress:         "127.0.0.1:8080",
		Cooldown:              time.Second,
		RecoveryWait:          time.Second,
		ResponseHeaderTimeout: time.Second,
		LogLevel:              logLevel,
		ReasoningEffort:       &reasoningMax,
		Providers:             configuredProviders,
	}, log.New(&logs, "", 0))
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	return httptest.NewServer(handler), &logs
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
	yaml.WriteString("reasoning_effort: max\n")
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

func assertRequestBody(t *testing.T, gotBody []byte, wantBody string, wantModelAlias string, wantReasoningEffort *string) {
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
	if wantReasoningEffort != nil {
		if got["reasoning_effort"] != *wantReasoningEffort {
			t.Errorf("upstream reasoning_effort = %v, want %q", got["reasoning_effort"], *wantReasoningEffort)
		}
	} else {
		if _, ok := got["reasoning_effort"]; ok {
			t.Errorf("upstream reasoning_effort = %v, want absent", got["reasoning_effort"])
		}
	}
	delete(got, "model")
	delete(got, "reasoning_effort")
	delete(want, "model")
	delete(want, "reasoning_effort")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upstream body fields = %#v, want %#v", got, want)
	}
}

func stringPtr(s string) *string { return &s }

func effortPtr(e config.ReasoningEffort) *config.ReasoningEffort { return &e }
