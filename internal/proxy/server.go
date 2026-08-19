package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
)

const chatCompletionsPath = "/v1/chat/completions"

// Logger is the operational logging surface used by the proxy.
type Logger interface {
	Printf(format string, args ...any)
}

// Server implements the public OpenAI-compatible HTTP endpoint.
type Server struct {
	providers        []*provider
	client           *http.Client
	cooldownDuration time.Duration
	recoveryWait     time.Duration
	cooldownMu       sync.Mutex
	cooldownVersion  uint64
	logger           Logger
}

type provider struct {
	config.Provider
	index           int
	chatURL         string
	cooldownUntil   time.Time
	cooldownVersion uint64
}

// New creates a proxy handler from validated configuration.
func New(cfg config.Config) (*Server, error) {
	return NewWithLogger(cfg, log.Default())
}

// NewWithLogger creates a proxy handler with an injected operational logger.
func NewWithLogger(cfg config.Config, logger Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid proxy config: %w", err)
	}
	if logger == nil {
		logger = log.Default()
	}

	providers := make([]*provider, 0, len(cfg.Providers))
	for index, configuredProvider := range cfg.Providers {
		chatURL, err := chatCompletionsURL(configuredProvider.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("Provider base URL: %w", err)
		}
		providers = append(providers, &provider{
			Provider: configuredProvider,
			index:    index,
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
		recoveryWait:     cfg.RecoveryWait,
		client: &http.Client{
			Transport: transport,
		},
		logger: logger,
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
			s.logCancellation("request-body")
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
	for {
		if r.Context().Err() != nil {
			s.logCancellation("routing")
			return
		}

		for _, selected := range s.providers {
			if r.Context().Err() != nil {
				s.logCancellation("routing")
				return
			}

			if !s.providerAvailable(selected, time.Now()) {
				continue
			}
			s.logger.Printf("provider selected provider=%d priority=%d", selected.index, selected.Priority)

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
					s.logCancellation("upstream")
					return
				}
				category := "network"
				if isResponseHeaderTimeout(err) {
					category = "response-header-timeout"
				}
				s.handleFailoverFailure(selected, category, 0)
				continue
			}

			if isFailoverStatus(upstreamResponse.StatusCode) {
				_ = upstreamResponse.Body.Close()
				category := "server-error"
				if upstreamResponse.StatusCode == http.StatusTooManyRequests {
					category = "rate-limit"
				}
				s.handleFailoverFailure(selected, category, upstreamResponse.StatusCode)
				continue
			}

			streaming := isStreamingResponse(upstreamResponse, payload)
			defer upstreamResponse.Body.Close()
			copyHeaders(w.Header(), upstreamResponse.Header)
			w.WriteHeader(upstreamResponse.StatusCode)
			if streaming {
				s.forwardStreamingResponse(w, r, selected, upstreamResponse.StatusCode, upstreamResponse.Body)
				return
			}
			_, _ = io.Copy(w, upstreamResponse.Body)
			return
		}

		if !s.waitForRecovery(r.Context()) {
			return
		}
	}
}

func (s *Server) waitForRecovery(ctx context.Context) bool {
	cooldownVersions := s.snapshotCooldownVersions()
	s.logger.Printf("recovery wait started duration=%s", s.recoveryWait)
	timer := time.NewTimer(s.recoveryWait)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	select {
	case <-ctx.Done():
		s.logger.Printf("recovery wait canceled")
		return false
	case <-timer.C:
		if ctx.Err() != nil {
			s.logger.Printf("recovery wait canceled")
			return false
		}
		cleared := s.clearCooldowns(cooldownVersions)
		s.logger.Printf("recovery wait completed cooldowns_cleared=%d", cleared)
		return true
	}
}

func (s *Server) snapshotCooldownVersions() map[*provider]uint64 {
	s.cooldownMu.Lock()
	defer s.cooldownMu.Unlock()

	versions := make(map[*provider]uint64, len(s.providers))
	for _, selected := range s.providers {
		versions[selected] = selected.cooldownVersion
	}
	return versions
}

func (s *Server) clearCooldowns(versions map[*provider]uint64) int {
	s.cooldownMu.Lock()
	cleared := make([]int, 0, len(s.providers))
	for _, selected := range s.providers {
		if selected.cooldownVersion != versions[selected] {
			continue
		}
		if selected.cooldownUntil.IsZero() {
			continue
		}
		selected.cooldownUntil = time.Time{}
		cleared = append(cleared, selected.index)
	}
	s.cooldownMu.Unlock()
	for _, index := range cleared {
		s.logger.Printf("cooldown cleared provider=%d", index)
	}
	return len(cleared)
}

func (s *Server) forwardStreamingResponse(w http.ResponseWriter, r *http.Request, selected *provider, statusCode int, body io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	buffer := make([]byte, 32*1024)
	for {
		readCount, readErr := body.Read(buffer)
		if readCount > 0 {
			chunk := buffer[:readCount]
			if _, writeErr := w.Write(chunk); writeErr != nil {
				if r.Context().Err() != nil {
					s.logCancellation("stream")
				}
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}

		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			return
		}
		if r.Context().Err() != nil {
			s.logCancellation("stream")
			return
		}
		s.handleFailoverFailure(selected, "stream", statusCode)
		return
	}
}

func isStreamingResponse(response *http.Response, payload map[string]json.RawMessage) bool {
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return false
	}

	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "text/event-stream") {
		return true
	}

	streamValue, ok := payload["stream"]
	if !ok {
		return false
	}
	var requested bool
	return json.Unmarshal(streamValue, &requested) == nil && requested
}

func (s *Server) providerAvailable(selected *provider, now time.Time) bool {
	s.cooldownMu.Lock()
	defer s.cooldownMu.Unlock()
	return !now.Before(selected.cooldownUntil)
}

func (s *Server) markCooldown(selected *provider) {
	cooldownUntil := time.Now().Add(s.cooldownDuration)
	s.cooldownMu.Lock()
	previousCooldownUntil := selected.cooldownUntil
	s.cooldownVersion = s.cooldownVersion + 1
	selected.cooldownVersion = s.cooldownVersion
	if cooldownUntil.After(selected.cooldownUntil) {
		selected.cooldownUntil = cooldownUntil
	}
	s.cooldownMu.Unlock()
	transition := "entered"
	if time.Now().Before(previousCooldownUntil) {
		transition = "extended"
	}
	s.logger.Printf("cooldown %s provider=%d duration=%s", transition, selected.index, s.cooldownDuration)
}

func (s *Server) handleFailoverFailure(selected *provider, category string, statusCode int) {
	if statusCode > 0 {
		s.logger.Printf("failover failure provider=%d category=%s status=%d", selected.index, category, statusCode)
	} else {
		s.logger.Printf("failover failure provider=%d category=%s", selected.index, category)
	}
	s.markCooldown(selected)
}

func (s *Server) logCancellation(phase string) {
	s.logger.Printf("request canceled phase=%s", phase)
}

func isResponseHeaderTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timeoutError interface{ Timeout() bool }
	return errors.As(err, &timeoutError) && timeoutError.Timeout()
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
