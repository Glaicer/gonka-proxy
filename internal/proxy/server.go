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

	"github.com/glaicer/gonka-proxy/internal/config"
)

const chatCompletionsPath = "/v1/chat/completions"

// Server implements the public OpenAI-compatible HTTP endpoint.
type Server struct {
	providers []provider
	client    *http.Client
}

type provider struct {
	config.Provider
	chatURL string
}

// New creates a proxy handler from validated configuration.
func New(cfg config.Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid proxy config: %w", err)
	}

	providers := make([]provider, 0, len(cfg.Providers))
	for _, configuredProvider := range cfg.Providers {
		chatURL, err := chatCompletionsURL(configuredProvider.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("Provider base URL: %w", err)
		}
		providers = append(providers, provider{
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
		providers: providers,
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

	selected := s.providers[0]
	upstreamBody, err := replaceVirtualModelWithAlias(body, selected.ModelAlias)
	if err != nil {
		http.Error(w, "request body must be a JSON object", http.StatusBadRequest)
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
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer upstreamResponse.Body.Close()

	copyHeaders(w.Header(), upstreamResponse.Header)
	w.WriteHeader(upstreamResponse.StatusCode)
	_, _ = io.Copy(w, upstreamResponse.Body)
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

func replaceVirtualModelWithAlias(body []byte, modelAlias string) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, fmt.Errorf("decode request body")
	}
	encodedModelAlias, err := json.Marshal(modelAlias)
	if err != nil {
		return nil, fmt.Errorf("encode model alias: %w", err)
	}
	payload["model"] = encodedModelAlias
	return json.Marshal(payload)
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
