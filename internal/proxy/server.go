package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
)

const chatCompletionsPath = "/v1/chat/completions"

// Server implements the public OpenAI-compatible HTTP endpoint.
type Server struct {
	providers        []*provider
	client           *http.Client
	cooldownDuration time.Duration
	cooldownMu       sync.Mutex
}

type provider struct {
	config.Provider
	chatURL       string
	cooldownUntil time.Time
}

// New creates a proxy handler from validated configuration.
func New(cfg config.Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid proxy config: %w", err)
	}

	providers := make([]*provider, 0, len(cfg.Providers))
	for _, configuredProvider := range cfg.Providers {
		chatURL, err := chatCompletionsURL(configuredProvider.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("Provider base URL: %w", err)
		}
		providers = append(providers, &provider{
			Provider: configuredProvider,
			chatURL:  chatURL,
		})
	}
	sort.SliceStable(providers, func(i, j int) bool {
		return providers[i].Priority > providers[j].Priority
	})

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("http.DefaultTransport is not an *http.Transport")
	}
	transport = transport.Clone()
	transport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout

	return &Server{
		providers:        providers,
		cooldownDuration: cfg.Cooldown,
		client: &http.Client{
			Transport: transport,
		},
	}, nil
}

// ServeHTTP handles the single MVP endpoint.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != chatCompletionsPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(s.providers) == 0 {
		http.Error(w, "no Providers configured", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	payload, err := decodeRequestBody(body)
	if err != nil {
		http.Error(w, "request body must be a JSON object", http.StatusBadRequest)
		return
	}

	s.route(w, r, payload)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, payload map[string]json.RawMessage) {
	for _, selected := range s.providers {
		if !s.providerAvailable(selected, time.Now()) {
			continue
		}

		upstreamBody, err := replaceVirtualModelWithAlias(payload, selected.ModelAlias)
		if err != nil {
			http.Error(w, "could not encode upstream request", http.StatusBadGateway)
			return
		}

		upstreamRequest, err := http.NewRequestWithContext(
			r.Context(),
			http.MethodPost,
			selected.chatURL,
			bytes.NewReader(upstreamBody),
		)
		if err != nil {
			http.Error(w, "could not create upstream request", http.StatusBadGateway)
			return
		}
		copyHeaders(upstreamRequest.Header, r.Header)
		upstreamRequest.Header.Set("Authorization", "Bearer "+selected.APIKey)

		upstreamResponse, err := s.client.Do(upstreamRequest)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			s.markCooldown(selected)
			continue
		}

		if isFailoverStatus(upstreamResponse.StatusCode) {
			_ = upstreamResponse.Body.Close()
			s.markCooldown(selected)
			continue
		}

		defer upstreamResponse.Body.Close()
		copyHeaders(w.Header(), upstreamResponse.Header)
		w.WriteHeader(upstreamResponse.StatusCode)
		_, _ = io.Copy(w, upstreamResponse.Body)
		return
	}

	if r.Context().Err() != nil {
		return
	}
	http.Error(w, "all Providers failed", http.StatusBadGateway)
}

func (s *Server) providerAvailable(selected *provider, now time.Time) bool {
	s.cooldownMu.Lock()
	defer s.cooldownMu.Unlock()
	return !now.Before(selected.cooldownUntil)
}

func (s *Server) markCooldown(selected *provider) {
	cooldownUntil := time.Now().Add(s.cooldownDuration)
	s.cooldownMu.Lock()
	if cooldownUntil.After(selected.cooldownUntil) {
		selected.cooldownUntil = cooldownUntil
	}
	s.cooldownMu.Unlock()
}

func isFailoverStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests ||
		(statusCode >= http.StatusInternalServerError && statusCode <= 599)
}

func chatCompletionsURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("base_url must be a valid URL: %w", err)
	}
	path := strings.TrimRight(parsed.Path, "/")
	parsed.Path = path + "/chat/completions"
	parsed.RawPath = ""
	return parsed.String(), nil
}

func decodeRequestBody(body []byte) (map[string]json.RawMessage, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, fmt.Errorf("decode request body")
	}
	return payload, nil
}

func replaceVirtualModelWithAlias(payload map[string]json.RawMessage, modelAlias string) ([]byte, error) {
	forwardedPayload := make(map[string]json.RawMessage, len(payload)+1)
	for key, value := range payload {
		forwardedPayload[key] = value
	}
	encodedModelAlias, err := json.Marshal(modelAlias)
	if err != nil {
		return nil, fmt.Errorf("encode model alias: %w", err)
	}
	forwardedPayload["model"] = encodedModelAlias
	return json.Marshal(forwardedPayload)
}

func copyHeaders(destination, source http.Header) {
	hopByHop := map[string]struct{}{
		"connection":          {},
		"keep-alive":          {},
		"proxy-authenticate":  {},
		"proxy-authorization": {},
		"te":                  {},
		"trailer":             {},
		"transfer-encoding":   {},
		"upgrade":             {},
	}
	for _, connectionHeader := range source.Values("Connection") {
		for _, token := range strings.Split(connectionHeader, ",") {
			hopByHop[strings.ToLower(strings.TrimSpace(token))] = struct{}{}
		}
	}

	for key, values := range source {
		if _, excluded := hopByHop[strings.ToLower(key)]; excluded {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}
