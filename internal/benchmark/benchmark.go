// Package benchmark contains the reusable provider benchmark runner.
package benchmark

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
)

// Options controls one benchmark run.
type Options struct {
	Rounds       int
	MaxTokens    int
	Timeout      time.Duration
	Delay        time.Duration
	PromptPrefix string
	// Providers restricts the run to the named providers; empty means all.
	Providers []string
	// Models restricts the run to the named models; empty means all.
	Models []string
	// Match keeps only models whose name contains it, case-insensitively.
	Match string
	// DiscoverOnly resolves model lists without sending chat requests.
	DiscoverOnly bool
	Now          func() time.Time
	Rand         func([]byte) (int, error)
}

// Model sources reported in ProviderResult.ModelSource.
const (
	// ModelSourceEndpoint means the list came from the provider /models endpoint.
	ModelSourceEndpoint = "endpoint"
	// ModelSourceConfig means the list came from the provider `models` config key.
	ModelSourceConfig = "config"
	// ModelSourceLegacy means the list came from model_alias/model_aliases.
	ModelSourceLegacy = "legacy"
)

// Report is a machine-readable benchmark result. It deliberately excludes
// provider URLs, API keys, prompts, and response bodies.
type Report struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Rounds      int              `json:"rounds"`
	MaxTokens   int              `json:"max_tokens"`
	Results     []ProviderResult `json:"results"`
}

// ProviderResult groups discovery and chat results for one configured provider.
type ProviderResult struct {
	Provider        string      `json:"provider"`
	Models          []string    `json:"models,omitempty"`
	ModelSource     string      `json:"model_source,omitempty"`
	DiscoveryStatus string      `json:"discovery_status"`
	DiscoveryError  string      `json:"discovery_error,omitempty"`
	Runs            []RunResult `json:"runs,omitempty"`
}

// RunResult is one model/round measurement.
type RunResult struct {
	Model              string    `json:"model"`
	Round              int       `json:"round"`
	StartedAt          time.Time `json:"started_at"`
	Status             string    `json:"status"`
	ErrorCategory      string    `json:"error_category,omitempty"`
	StatusCode         int       `json:"status_code,omitempty"`
	HeaderLatencyMS    int64     `json:"header_latency_ms,omitempty"`
	FirstByteLatencyMS int64     `json:"first_byte_latency_ms,omitempty"`
	TotalLatencyMS     int64     `json:"total_latency_ms"`
	ResponseBytes      int64     `json:"response_bytes,omitempty"`
}

// Run resolves one model list per provider and tests each model sequentially.
func Run(ctx context.Context, cfg config.Config, options Options) (Report, error) {
	options = options.withDefaults()
	report := Report{
		GeneratedAt: options.Now().UTC(),
		Rounds:      options.Rounds,
		MaxTokens:   options.MaxTokens,
		Results:     make([]ProviderResult, 0, len(cfg.Providers)),
	}

	client := &http.Client{Timeout: options.Timeout}
	for _, provider := range cfg.Providers {
		if !options.wantsProvider(provider.Name) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		result := ProviderResult{Provider: provider.Name, DiscoveryStatus: "error"}
		models, source, discoveryErr := resolveModels(ctx, client, provider)
		if discoveryErr != nil {
			result.DiscoveryError = classifyError(discoveryErr)
		} else {
			result.DiscoveryStatus = "success"
		}
		models = options.filterModels(models)
		if options.hasModelFilter() && len(models) == 0 && !options.DiscoverOnly {
			continue
		}
		result.ModelSource = source
		result.Models = models

		if options.DiscoverOnly {
			report.Results = append(report.Results, result)
			continue
		}

		for _, model := range models {
			for round := 1; round <= options.Rounds; round++ {
				if err := ctx.Err(); err != nil {
					return report, err
				}
				run := testModel(ctx, client, provider, model, round, options)
				result.Runs = append(result.Runs, run)
				if options.Delay > 0 && (round != options.Rounds || model != models[len(models)-1]) {
					select {
					case <-ctx.Done():
						return report, ctx.Err()
					case <-time.After(options.Delay):
					}
				}
			}
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func (o Options) withDefaults() Options {
	if o.Rounds <= 0 {
		o.Rounds = 3
	}
	if o.MaxTokens <= 0 {
		o.MaxTokens = 8
	}
	if o.Timeout <= 0 {
		o.Timeout = 45 * time.Second
	}
	if o.Delay < 0 {
		o.Delay = 0
	}
	if o.PromptPrefix == "" {
		o.PromptPrefix = "Return exactly OK followed by the nonce."
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Read
	}
	return o
}

// resolveModels picks the model list for one provider. A working /models
// endpoint wins; otherwise the configured `models` list is used, then the
// legacy model_alias/model_aliases values. The endpoint error, when any, is
// returned alongside the fallback list so it can be reported.
func resolveModels(ctx context.Context, client *http.Client, provider config.Provider) ([]string, string, error) {
	models, err := discoverModels(ctx, client, provider)
	if err == nil {
		return models, ModelSourceEndpoint, nil
	}
	if configured := providerModels(provider); len(configured) > 0 {
		return configured, ModelSourceConfig, err
	}
	legacy := legacyModels(provider)
	if len(legacy) == 0 {
		return nil, "", err
	}
	return legacy, ModelSourceLegacy, err
}

func (o Options) wantsProvider(name string) bool {
	if len(o.Providers) == 0 {
		return true
	}
	for _, wanted := range o.Providers {
		if wanted == name {
			return true
		}
	}
	return false
}

func (o Options) hasModelFilter() bool {
	return len(o.Models) > 0 || strings.TrimSpace(o.Match) != ""
}

func (o Options) filterModels(models []string) []string {
	if !o.hasModelFilter() || len(models) == 0 {
		return models
	}
	wanted := make(map[string]struct{}, len(o.Models))
	for _, model := range o.Models {
		wanted[model] = struct{}{}
	}
	match := strings.ToLower(strings.TrimSpace(o.Match))
	filtered := make([]string, 0, len(models))
	for _, model := range models {
		if len(wanted) > 0 {
			if _, ok := wanted[model]; !ok {
				continue
			}
		}
		if match != "" && !strings.Contains(strings.ToLower(model), match) {
			continue
		}
		filtered = append(filtered, model)
	}
	return filtered
}

func discoverModels(ctx context.Context, client *http.Client, provider config.Provider) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(provider.BaseURL, "models"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: models returned HTTP %d", classifyHTTPStatus(response.StatusCode), response.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	models := make([]string, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for _, item := range payload.Data {
		model := strings.TrimSpace(item.ID)
		if model != "" {
			if _, ok := seen[model]; !ok {
				models = append(models, model)
				seen[model] = struct{}{}
			}
		}
	}
	sort.Strings(models)
	if len(models) == 0 {
		return nil, fmt.Errorf("models response contained no model IDs")
	}
	return models, nil
}

// providerModels returns the explicit upstream model list from the config.
func providerModels(provider config.Provider) []string {
	if len(provider.Models) == 0 {
		return nil
	}
	models := make([]string, 0, len(provider.Models))
	seen := make(map[string]struct{}, len(provider.Models))
	for _, model := range provider.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return models
}

// legacyModels falls back to the alias values when neither the endpoint nor an
// explicit `models` list is available. model_aliases maps a Virtual Model to an
// upstream name, so the upstream values are what the benchmark must send.
func legacyModels(provider config.Provider) []string {
	seen := map[string]struct{}{}
	models := make([]string, 0, len(provider.ModelAliases)+1)
	if model := strings.TrimSpace(provider.ModelAlias); model != "" {
		models = append(models, model)
		seen[model] = struct{}{}
	}
	for _, alias := range provider.ModelAliases {
		alias = strings.TrimSpace(alias)
		if alias != "" {
			if _, ok := seen[alias]; !ok {
				models = append(models, alias)
				seen[alias] = struct{}{}
			}
		}
	}
	sort.Strings(models)
	return models
}

func testModel(ctx context.Context, client *http.Client, provider config.Provider, model string, round int, options Options) RunResult {
	started := options.Now().UTC()
	result := RunResult{Model: model, Round: round, StartedAt: started, Status: "error"}
	nonceBytes := make([]byte, 8)
	if _, err := options.Rand(nonceBytes); err != nil {
		result.ErrorCategory = "nonce"
		return result
	}
	nonce := fmt.Sprintf("%d-%s-%d-%s", started.UnixNano(), model, round, hex.EncodeToString(nonceBytes))
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{{
			"role":    "user",
			"content": options.PromptPrefix + " Test nonce: " + nonce,
		}},
		"max_tokens":  options.MaxTokens,
		"temperature": 0,
		"stream":      false,
	})
	if err != nil {
		result.ErrorCategory = "request-encode"
		return result
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(provider.BaseURL, "chat/completions"), strings.NewReader(string(body)))
	if err != nil {
		result.ErrorCategory = "request-create"
		return result
	}
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	req.Header.Set("Content-Type", "application/json")
	requestStarted := time.Now()
	response, err := client.Do(req)
	if err != nil {
		result.TotalLatencyMS = time.Since(requestStarted).Milliseconds()
		result.ErrorCategory = classifyError(err)
		return result
	}
	result.HeaderLatencyMS = time.Since(requestStarted).Milliseconds()
	result.StatusCode = response.StatusCode
	firstByteAt := time.Time{}
	reader := &firstByteReader{reader: response.Body, onFirstByte: func() { firstByteAt = time.Now() }}
	responseBody, readErr := io.ReadAll(reader)
	response.Body.Close()
	result.TotalLatencyMS = time.Since(requestStarted).Milliseconds()
	if !firstByteAt.IsZero() {
		result.FirstByteLatencyMS = firstByteAt.Sub(requestStarted).Milliseconds()
	}
	result.ResponseBytes = int64(len(responseBody))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result.ErrorCategory = classifyHTTPStatus(response.StatusCode)
		return result
	}
	if readErr != nil {
		result.ErrorCategory = classifyError(readErr)
		return result
	}
	var payload struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(responseBody, &payload); err != nil || len(payload.Choices) == 0 {
		result.ErrorCategory = "invalid-response"
		return result
	}
	result.Status = "success"
	return result
}

type firstByteReader struct {
	reader      io.Reader
	onFirstByte func()
	seen        bool
}

func (r *firstByteReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if n > 0 && !r.seen {
		r.seen = true
		r.onFirstByte()
	}
	return n, err
}

func endpoint(baseURL, path string) string {
	return strings.TrimRight(baseURL, "/") + "/" + path
}

func classifyHTTPStatus(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate-limit"
	case status == http.StatusPaymentRequired:
		return "payment-required"
	case status >= 500:
		return "server-error"
	case status >= 400:
		return "client-error"
	default:
		return "http-error"
	}
}

func classifyError(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return "timeout"
	}
	for _, category := range []string{"rate-limit", "payment-required", "server-error", "client-error", "http-error"} {
		if strings.Contains(err.Error(), category) {
			return category
		}
	}
	return "transport-error"
}
