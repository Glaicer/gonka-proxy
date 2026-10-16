package proxy_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
	"github.com/glaicer/gonka-proxy/internal/proxy"
)

func TestChatCompletionsCancellationPreservesProviderAvailability(t *testing.T) {
	for _, level := range []config.LogLevel{config.LogLevelInfo, config.LogLevelWarn, config.LogLevelError} {
		t.Run(string(level), func(t *testing.T) {
			var primaryHits, backupHits atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if primaryHits.Add(1) == 1 {
					_, _ = io.WriteString(w, `{"choices":[`)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"primary"},"finish_reason":"stop"}]}`)
			}))
			defer primary.Close()
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backupHits.Add(1)
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"backup"},"finish_reason":"stop"}]}`)
			}))
			defer backup.Close()
			var logs bytes.Buffer
			started := make(chan struct{}, 1)
			logger := cancellationLogger(func(format string, args ...any) {
				message := fmt.Sprintf(format, args...)
				logs.WriteString(message + "\n")
				if message == "primary status=200" {
					select {
					case started <- struct{}{}:
					default:
					}
				}
			})
			handler, err := proxy.NewWithLogger(config.Config{
				ListenAddress: "127.0.0.1:8080", Cooldown: time.Minute, RecoveryWait: time.Second,
				ResponseHeaderTimeout: time.Second, LogLevel: level,
				Providers: []config.Provider{
					{Name: "primary", BaseURL: primary.URL + "/v1", APIKey: "primary-secret", ModelAlias: "primary-model", Priority: 100},
					{Name: "backup", BaseURL: backup.URL + "/v1", APIKey: "backup-secret", ModelAlias: "backup-model", Priority: 50},
				},
			}, logger)
			if err != nil {
				t.Fatalf("create proxy: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`)).WithContext(ctx)
				handler.ServeHTTP(httptest.NewRecorder(), request)
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("proxy did not begin consuming the upstream response")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("response-body read did not stop after cancellation")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`)))
			if primaryHits.Load() != 2 || backupHits.Load() != 0 || !strings.Contains(response.Body.String(), "primary") {
				t.Errorf("healthy Provider was cooled down: primary=%d backup=%d response=%s", primaryHits.Load(), backupHits.Load(), response.Body.String())
			}
			output := logs.String()
			if strings.Contains(output, "failover") || strings.Contains(output, "cooldown") {
				t.Errorf("cancellation logged as Provider failure: %s", output)
			}
			if strings.Contains(output, "request canceled - upstream-body") != (level == config.LogLevelInfo) {
				t.Errorf("unexpected cancellation logging at %s: %s", level, output)
			}
		})
	}
}

type cancellationLogger func(string, ...any)

func (l cancellationLogger) Printf(format string, args ...any) { l(format, args...) }
