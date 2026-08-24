package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunShutsDownActiveRoutingOnContextCancellation(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		startedOnce.Do(func() {
			close(upstreamStarted)
		})
		<-r.Context().Done()
		canceledOnce.Do(func() {
			close(upstreamCanceled)
		})
	}))
	defer provider.Close()

	listenAddress := reserveTCPAddress(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf(`server:
  listen_address: %s
cooldown: 1h
recovery_wait: 1h
response_header_timeout: 1h
reasoning_effort: max
providers:
  - name: primary
    base_url: %s/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 1
`, listenAddress, provider.URL)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	runResult := make(chan error, 1)
	go func() {
		runResult <- run(ctx, configPath, log.New(&logs, "", 0))
	}()
	waitForTCP(t, listenAddress)

	requestResult := make(chan error, 1)
	go func() {
		request, err := http.NewRequest(http.MethodPost, "http://"+listenAddress+"/v1/chat/completions", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
		if err != nil {
			requestResult <- err
			return
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		requestResult <- err
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active upstream routing")
	}
	cancel()

	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatalf("shutdown did not cancel the active upstream request; logs = %q", logs.String())
	}
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not finish after cancellation")
	}
	select {
	case <-requestResult:
	case <-time.After(time.Second):
		t.Fatal("downstream request did not finish after shutdown")
	}
}

func reserveTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release TCP address: %v", err)
	}
	return address
}

func waitForTCP(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", address)
}
