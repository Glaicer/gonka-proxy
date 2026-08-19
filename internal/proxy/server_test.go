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
	"sync"
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
				req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
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
	return newProxyServerWithTimingValues(t, providers, "", "")
}

func newProxyServerWithTiming(t *testing.T, providers []providerFixture, cooldown, responseHeaderTimeout time.Duration) *httptest.Server {
	t.Helper()
	return newProxyServerWithTimingValues(t, providers, cooldown.String(), responseHeaderTimeout.String())
}

func newProxyServerWithTimingValues(t *testing.T, providers []providerFixture, cooldown, responseHeaderTimeout string) *httptest.Server {
	t.Helper()

	var yaml strings.Builder
	if cooldown != "" {
		fmt.Fprintf(&yaml, "cooldown: %s\n", cooldown)
	}
	if responseHeaderTimeout != "" {
		fmt.Fprintf(&yaml, "response_header_timeout: %s\n", responseHeaderTimeout)
	}
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
