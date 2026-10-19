package benchmark

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
)

func TestRunDiscoversAndTestsEveryModelWithUniquePrompts(t *testing.T) {
	var prompts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-b"},{"id":"model-a"}]}`))
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var payload struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		prompts = append(prompts, payload.Messages[0].Content)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer server.Close()

	report, err := Run(context.Background(), config.Config{Providers: []config.Provider{{
		Name: "test", BaseURL: server.URL + "/v1", APIKey: "secret", ModelAlias: "fallback", Priority: 1,
	}}}, Options{Rounds: 2, Delay: 0, Now: func() time.Time { return time.Unix(100, 0) }, Rand: func(buffer []byte) (int, error) {
		for i := range buffer {
			buffer[i] = byte(i + 1)
		}
		return len(buffer), nil
	}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(report.Results) != 1 || len(report.Results[0].Models) != 2 || len(report.Results[0].Runs) != 4 {
		t.Fatalf("report = %#v, want one provider, two models, four runs", report)
	}
	if report.Results[0].Models[0] != "model-a" || report.Results[0].Models[1] != "model-b" {
		t.Fatalf("models = %#v, want sorted discovery order", report.Results[0].Models)
	}
	if len(prompts) != 4 || prompts[0] == prompts[1] || prompts[0] == prompts[2] {
		t.Fatalf("prompts are not unique: %#v", prompts)
	}
	for _, run := range report.Results[0].Runs {
		if run.Status != "success" || run.Model == "" || run.TotalLatencyMS < 0 {
			t.Fatalf("run = %#v, want success", run)
		}
	}
}

func TestRunFallsBackToConfiguredModelsWhenDiscoveryFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.Error(w, "not supported", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer server.Close()

	report, err := Run(context.Background(), config.Config{Providers: []config.Provider{{
		Name: "test", BaseURL: server.URL + "/v1", APIKey: "secret", ModelAlias: "legacy", ModelAliases: map[string]string{"virtual": "upstream"}, Priority: 1,
	}}}, Options{Rounds: 1, Delay: 0})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	result := report.Results[0]
	if result.DiscoveryStatus != "error" || !strings.Contains(result.DiscoveryError, "client-error") {
		t.Fatalf("discovery = %#v, want client-error", result)
	}
	if result.ModelSource != ModelSourceLegacy {
		t.Fatalf("model_source = %q, want %q", result.ModelSource, ModelSourceLegacy)
	}
	if len(result.Models) != 2 || len(result.Runs) != 2 {
		t.Fatalf("result = %#v, want two configured fallback models and runs", result)
	}
	if result.Models[0] != "legacy" || result.Models[1] != "upstream" {
		t.Fatalf("models = %#v, want upstream alias names", result.Models)
	}
}

func TestRunUsesExplicitModelListWhenDiscoveryFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer server.Close()

	report, err := Run(context.Background(), config.Config{Providers: []config.Provider{{
		Name: "test", BaseURL: server.URL + "/v1", APIKey: "secret",
		ModelAlias: "legacy", Models: []string{"model-b", "model-a"}, Priority: 1,
	}}}, Options{Rounds: 1, Delay: 0})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	result := report.Results[0]
	if result.DiscoveryStatus != "error" {
		t.Fatalf("discovery = %#v, want error", result)
	}
	if result.ModelSource != ModelSourceConfig {
		t.Fatalf("model_source = %q, want %q", result.ModelSource, ModelSourceConfig)
	}
	if len(result.Models) != 2 || result.Models[0] != "model-b" || result.Models[1] != "model-a" {
		t.Fatalf("models = %#v, want the configured list in order", result.Models)
	}
	if len(result.Runs) != 2 {
		t.Fatalf("runs = %d, want one per configured model", len(result.Runs))
	}
}

func TestRunPrefersEndpointOverExplicitModelList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"endpoint-model"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer server.Close()

	report, err := Run(context.Background(), config.Config{Providers: []config.Provider{{
		Name: "test", BaseURL: server.URL + "/v1", APIKey: "secret",
		ModelAlias: "legacy", Models: []string{"configured-model"}, Priority: 1,
	}}}, Options{Rounds: 1, Delay: 0})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	result := report.Results[0]
	if result.DiscoveryStatus != "success" {
		t.Fatalf("discovery = %#v, want success", result)
	}
	if result.ModelSource != ModelSourceEndpoint {
		t.Fatalf("model_source = %q, want %q", result.ModelSource, ModelSourceEndpoint)
	}
	if len(result.Models) != 1 || result.Models[0] != "endpoint-model" {
		t.Fatalf("models = %#v, want the endpoint list", result.Models)
	}
}

func TestRunDiscoverOnlySendsNoChatRequests(t *testing.T) {
	chatRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
			return
		}
		chatRequests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer server.Close()

	report, err := Run(context.Background(), config.Config{Providers: []config.Provider{{
		Name: "test", BaseURL: server.URL + "/v1", APIKey: "secret", ModelAlias: "legacy", Priority: 1,
	}}}, Options{DiscoverOnly: true, Match: "no-such-model"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if chatRequests != 0 {
		t.Fatalf("chat requests = %d, want 0", chatRequests)
	}
	if len(report.Results) != 1 || report.Results[0].DiscoveryStatus != "success" {
		t.Fatalf("results = %#v, want the provider reported with successful discovery", report.Results)
	}
	if len(report.Results[0].Runs) != 0 || len(report.Results[0].Models) != 0 {
		t.Fatalf("result = %#v, want no runs and no models after the match filter", report.Results[0])
	}
}

func TestRunMatchesModelNamesCaseInsensitively(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"DeepSeek-V4-Flash-0731"},{"id":"MiniMaxAI/MiniMax-M2.7"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer server.Close()

	report, err := Run(context.Background(), config.Config{Providers: []config.Provider{{
		Name: "test", BaseURL: server.URL + "/v1", APIKey: "secret", ModelAlias: "legacy", Priority: 1,
	}}}, Options{Rounds: 1, Delay: 0, Match: "flash"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	result := report.Results[0]
	if len(result.Models) != 1 || result.Models[0] != "DeepSeek-V4-Flash-0731" {
		t.Fatalf("models = %#v, want only the flash model", result.Models)
	}
	if len(result.Runs) != 1 {
		t.Fatalf("runs = %d, want one", len(result.Runs))
	}
}

func TestRunFiltersProvidersAndModels(t *testing.T) {
	modelsServed := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		modelsServed[payload.Model]++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer server.Close()

	providers := []config.Provider{
		{Name: "primary", BaseURL: server.URL + "/v1", APIKey: "secret", ModelAlias: "legacy", Priority: 2},
		{Name: "backup", BaseURL: server.URL + "/v1", APIKey: "secret", ModelAlias: "legacy", Priority: 1},
	}
	report, err := Run(context.Background(), config.Config{Providers: providers}, Options{
		Rounds: 1, Delay: 0, Providers: []string{"backup"}, Models: []string{"model-b"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(report.Results) != 1 || report.Results[0].Provider != "backup" {
		t.Fatalf("results = %#v, want only the filtered provider", report.Results)
	}
	result := report.Results[0]
	if len(result.Models) != 1 || result.Models[0] != "model-b" {
		t.Fatalf("models = %#v, want only the filtered model", result.Models)
	}
	if len(result.Runs) != 1 || modelsServed["model-a"] != 0 || modelsServed["model-b"] != 1 {
		t.Fatalf("runs = %#v, served = %#v, want one request for model-b only", result.Runs, modelsServed)
	}
}
