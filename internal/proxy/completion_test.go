package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChatCompletionsValidatesFinishReasons(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		failover bool
	}{
		{"empty_reason", `{"choices":[{"message":{"content":""},"finish_reason":""}]}`, true},
		{"unknown_reason", `{"choices":[{"message":{"content":""},"finish_reason":"unknown"}]}`, true},
		{"unknown_reason_without_message", `{"choices":[{"finish_reason":"unknown"}]}`, true},
		{"empty_reason_with_delta", `{"choices":[{"delta":{"content":""},"finish_reason":""}]}`, true},
		{"content_with_null_reason", `{"choices":[{"message":{"content":"hello"},"finish_reason":null}]}`, false},
		{"content_with_unknown_reason", `{"choices":[{"message":{"content":"hello"},"finish_reason":"unknown"}]}`, false},
		{"tool_calls_with_null_reason", `{"choices":[{"message":{"tool_calls":[{"id":"call-1","type":"function","function":{"name":"demo","arguments":"{}"}}]},"finish_reason":null}]}`, false},
		{"stop", `{"choices":[{"finish_reason":"stop"}]}`, false},
		{"length", `{"choices":[{"finish_reason":"length"}]}`, false},
		{"content_filter", `{"choices":[{"finish_reason":"content_filter"}]}`, false},
		{"tool_calls", `{"choices":[{"finish_reason":"tool_calls"}]}`, false},
		{"legacy_function_call", `{"choices":[{"message":{"function_call":{"name":"demo","arguments":"{}"}},"finish_reason":"function_call"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var primaryHits, backupHits atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryHits.Add(1)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer primary.Close()
			const backupBody = `{"choices":[{"message":{"content":"backup"},"finish_reason":"stop"}]}`
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backupHits.Add(1)
				_, _ = io.WriteString(w, backupBody)
			}))
			defer backup.Close()
			server := newProxyServer(t, []providerFixture{
				{baseURL: primary.URL + "/v1", apiKey: "primary-secret", modelAlias: "primary-model", priority: 100},
				{baseURL: backup.URL + "/v1", apiKey: "backup-secret", modelAlias: "backup-model", priority: 50},
			})
			defer server.Close()
			wantBody := tc.body
			if tc.failover {
				wantBody = backupBody
			}
			for attempt := 0; attempt < 2; attempt++ {
				response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
				if err != nil {
					t.Fatalf("proxy request: %v", err)
				}
				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if readErr != nil {
					t.Fatalf("read response: %v", readErr)
				}
				if response.StatusCode != http.StatusOK || string(body) != wantBody {
					t.Fatalf("response = %d %s, want 200 %s", response.StatusCode, body, wantBody)
				}
			}
			wantPrimary, wantBackup := int32(2), int32(0)
			if tc.failover {
				wantPrimary, wantBackup = 1, 2
			}
			if primaryHits.Load() != wantPrimary || backupHits.Load() != wantBackup {
				t.Fatalf("Provider hits: primary=%d backup=%d, want %d/%d", primaryHits.Load(), backupHits.Load(), wantPrimary, wantBackup)
			}
		})
	}
}
