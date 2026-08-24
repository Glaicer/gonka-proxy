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

const maxLoggedErrorMessageLength = 512

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
	logLevel         config.LogLevel
	reasoningEffort  *config.ReasoningEffort
}

type provider struct {
	config.Provider
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

	// Normalize programmatic Config values (e.g. "MAX") to lower; Load already normalizes.
	var normalizedEffort *config.ReasoningEffort
	if cfg.ReasoningEffort != nil {
		n := config.ReasoningEffort(strings.ToLower(strings.TrimSpace(string(*cfg.ReasoningEffort))))
		normalizedEffort = &n
	}

	return &Server{
		providers:        providers,
		cooldownDuration: cfg.Cooldown,
		recoveryWait:     cfg.RecoveryWait,
		client: &http.Client{
			Transport: transport,
		},
		logger:         logger,
		logLevel:       cfg.LogLevel,
		reasoningEffort: normalizedEffort,
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
			s.logAt(config.LogLevelInfo, "provider selected - %s - priority=%d", selected.Name, selected.Priority)

			upstreamBody, err := applyUpstreamOverrides(payload, selected.ModelAlias, s.reasoningEffort)
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
				s.logProviderResponse(selected, 0, err.Error())
				category := "network"
				if isResponseHeaderTimeout(err) {
					category = "response-header-timeout"
				}
				s.handleFailoverFailure(selected, category, 0)
				continue
			}

			if upstreamResponse.StatusCode != http.StatusOK {
				responseBody, readErr := io.ReadAll(upstreamResponse.Body)
				_ = upstreamResponse.Body.Close()
				errorMessage := errorMessageFromResponse(responseBody, upstreamResponse.StatusCode)
				if readErr != nil {
					errorMessage = fmt.Sprintf("read upstream error response: %v", readErr)
				}
				s.logProviderResponse(selected, upstreamResponse.StatusCode, errorMessage)

				if isFailoverStatus(upstreamResponse.StatusCode) {
					category := "server-error"
					if upstreamResponse.StatusCode == http.StatusTooManyRequests {
						category = "rate-limit"
					}
					s.handleFailoverFailure(selected, category, upstreamResponse.StatusCode)
					continue
				}

				copyHeaders(w.Header(), upstreamResponse.Header)
				w.WriteHeader(upstreamResponse.StatusCode)
				_, _ = w.Write(responseBody)
				return
			}

			s.logProviderResponse(selected, upstreamResponse.StatusCode, "")

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
	s.logAt(config.LogLevelInfo, "recovery wait - started - %s", s.recoveryWait)
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
		s.logAt(config.LogLevelInfo, "recovery wait - canceled")
		return false
	case <-timer.C:
		if ctx.Err() != nil {
			s.logAt(config.LogLevelInfo, "recovery wait - canceled")
			return false
		}
		cleared := s.clearCooldowns(cooldownVersions)
		s.logAt(config.LogLevelInfo, "recovery wait - completed - cleared=%d", cleared)
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
	cleared := make([]*provider, 0, len(s.providers))
	for _, selected := range s.providers {
		if selected.cooldownVersion != versions[selected] {
			continue
		}
		if selected.cooldownUntil.IsZero() {
			continue
		}
		selected.cooldownUntil = time.Time{}
		cleared = append(cleared, selected)
	}
	s.cooldownMu.Unlock()
	for _, selected := range cleared {
		s.logAt(config.LogLevelInfo, "%s - cooldown cleared", selected.Name)
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
	s.cooldownVersion = s.cooldownVersion + 1
	selected.cooldownVersion = s.cooldownVersion
	if cooldownUntil.After(selected.cooldownUntil) {
		selected.cooldownUntil = cooldownUntil
	}
	s.cooldownMu.Unlock()
	s.logAt(config.LogLevelInfo, "%s - cooldown - %s", selected.Name, s.cooldownDuration)
}

func (s *Server) handleFailoverFailure(selected *provider, category string, statusCode int) {
	if statusCode > 0 {
		s.logAt(config.LogLevelWarn, "%s - failover - %s - %d", selected.Name, category, statusCode)
	} else {
		s.logAt(config.LogLevelWarn, "%s - failover - %s", selected.Name, category)
	}
	s.markCooldown(selected)
}

func (s *Server) logCancellation(phase string) {
	s.logAt(config.LogLevelInfo, "request canceled - %s", phase)
}

// logf writes a line unconditionally (used for always-on operational output).
func (s *Server) logf(format string, args ...any) {
	s.logger.Printf(format, args...)
}

// logAt writes a line only if the configured level permits it.
func (s *Server) logAt(level config.LogLevel, format string, args ...any) {
	if !s.logLevel.Enabled(level) {
		return
	}
	s.logger.Printf(format, args...)
}

func (s *Server) logProviderResponse(selected *provider, statusCode int, errorMessage string) {
	if errorMessage == "" {
		s.logf("%s status=%d", selected.Name, statusCode)
		return
	}
	s.logAt(config.LogLevelError, "%s error - status=%d - %s", selected.Name, statusCode, sanitizeErrorMessage(errorMessage))
}

func errorMessageFromResponse(body []byte, statusCode int) string {
	var payload struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if len(payload.Error) > 0 && string(payload.Error) != "null" {
			var errorText string
			if json.Unmarshal(payload.Error, &errorText) == nil && strings.TrimSpace(errorText) != "" {
				return sanitizeErrorMessage(errorText)
			}

			var structuredError struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(payload.Error, &structuredError) == nil && strings.TrimSpace(structuredError.Message) != "" {
				return sanitizeErrorMessage(structuredError.Message)
			}
		}
		if strings.TrimSpace(payload.Message) != "" {
			return sanitizeErrorMessage(payload.Message)
		}
		if strings.TrimSpace(payload.Detail) != "" {
			return sanitizeErrorMessage(payload.Detail)
		}
	}

	return fmt.Sprintf("upstream returned HTTP %d", statusCode)
}

func sanitizeErrorMessage(message string) string {
	message = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		default:
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > maxLoggedErrorMessageLength {
		message = message[:maxLoggedErrorMessageLength] + "..."
	}
	return message
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

func applyUpstreamOverrides(payload map[string]json.RawMessage, modelAlias string, reasoningEffort *config.ReasoningEffort) ([]byte, error) {
	forwardedPayload := make(map[string]json.RawMessage, len(payload)+2)
	for key, value := range payload {
		forwardedPayload[key] = value
	}
	encodedModelAlias, err := json.Marshal(modelAlias)
	if err != nil {
		return nil, fmt.Errorf("encode model alias: %w", err)
	}
	forwardedPayload["model"] = encodedModelAlias
	if reasoningEffort != nil {
		encodedEffort, err := json.Marshal(*reasoningEffort)
		if err != nil {
			return nil, fmt.Errorf("encode reasoning effort: %w", err)
		}
		forwardedPayload["reasoning_effort"] = encodedEffort
	} else {
		delete(forwardedPayload, "reasoning_effort")
	}
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
