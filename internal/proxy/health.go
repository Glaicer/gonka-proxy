package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	healthHistoryVersion       = 1
	healthHistoryMaxEvents     = 1000
	healthHistoryRetention     = 30 * 24 * time.Hour
	healthRecentLatencySamples = 20
	healthPersistInterval      = time.Second
)

type healthStore struct {
	mu                 sync.Mutex
	path               string
	providers          map[string]*healthProviderState
	events             []healthEvent
	logger             Logger
	persistenceWarning bool
	lastPersist        time.Time
}

type healthProviderState struct {
	Successes           uint64    `json:"successes"`
	Errors              uint64    `json:"errors"`
	LastSuccess         time.Time `json:"last_success,omitempty"`
	LastError           time.Time `json:"last_error,omitempty"`
	LastErrorCategory   string    `json:"last_error_category,omitempty"`
	LastErrorStatus     int       `json:"last_error_status,omitempty"`
	LastTransition      time.Time `json:"last_transition,omitempty"`
	LastTransitionType  string    `json:"last_transition_type,omitempty"`
	CooldownUntil       time.Time `json:"cooldown_until,omitempty"`
	CooldownTransitions uint64    `json:"cooldown_transitions"`
	LatencyCount        uint64    `json:"latency_count"`
	LatencySum          float64   `json:"latency_sum"`
	RecentLatencies     []float64 `json:"recent_latencies,omitempty"`
	LastLatencySeconds  float64   `json:"last_latency_seconds,omitempty"`
}

type healthEvent struct {
	At            time.Time `json:"at"`
	Provider      string    `json:"provider"`
	Type          string    `json:"type"`
	Category      string    `json:"category,omitempty"`
	Status        int       `json:"status,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
}

type healthHistoryFile struct {
	Version   int                             `json:"version"`
	SavedAt   time.Time                       `json:"saved_at"`
	Providers map[string]*healthProviderState `json:"providers"`
	Events    []healthEvent                   `json:"events,omitempty"`
}

func newHealthStore(path string, providerNames []string, logger Logger) *healthStore {
	if logger == nil {
		logger = log.Default()
	}
	store := &healthStore{
		path:      strings.TrimSpace(path),
		providers: make(map[string]*healthProviderState, len(providerNames)),
		logger:    logger,
	}
	store.load()
	for _, name := range providerNames {
		store.provider(name)
	}
	return store
}

func (h *healthStore) provider(name string) *healthProviderState {
	provider := h.providers[name]
	if provider == nil {
		provider = &healthProviderState{}
		h.providers[name] = provider
	}
	return provider
}

func (h *healthStore) recordSuccess(name string, at time.Time, latencySeconds float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	provider := h.provider(name)
	provider.Successes++
	provider.LastSuccess = at
	provider.LastTransition = at
	provider.LastTransitionType = "recovered"
	provider.CooldownUntil = time.Time{}
	provider.LatencyCount++
	provider.LatencySum += latencySeconds
	provider.LastLatencySeconds = latencySeconds
	provider.RecentLatencies = append(provider.RecentLatencies, latencySeconds)
	if len(provider.RecentLatencies) > healthRecentLatencySamples {
		provider.RecentLatencies = provider.RecentLatencies[len(provider.RecentLatencies)-healthRecentLatencySamples:]
	}
	h.events = append(h.events, healthEvent{At: at, Provider: name, Type: "success"})
	h.persistLocked(at, false)
}

func (h *healthStore) recordError(name, category string, statusCode int, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	provider := h.provider(name)
	provider.Errors++
	provider.LastError = at
	provider.LastErrorCategory = category
	provider.LastErrorStatus = statusCode
	provider.LastTransition = at
	provider.LastTransitionType = "error"
	h.events = append(h.events, healthEvent{At: at, Provider: name, Type: "error", Category: category, Status: statusCode})
	h.persistLocked(at, true)
}

func (h *healthStore) recordCooldown(name string, until, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	provider := h.provider(name)
	if until.After(provider.CooldownUntil) {
		provider.CooldownUntil = until
	}
	provider.CooldownTransitions++
	provider.LastTransition = at
	provider.LastTransitionType = "cooldown-started"
	h.events = append(h.events, healthEvent{At: at, Provider: name, Type: "cooldown-started", CooldownUntil: until})
	h.persistLocked(at, true)
}

func (h *healthStore) recordCooldownCleared(name string, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	provider := h.provider(name)
	provider.CooldownUntil = time.Time{}
	provider.LastTransition = at
	provider.LastTransitionType = "cooldown-cleared"
	h.events = append(h.events, healthEvent{At: at, Provider: name, Type: "cooldown-cleared"})
	h.persistLocked(at, true)
}

func (h *healthStore) serveHTTP(w io.Writer) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	names := make([]string, 0, len(h.providers))
	for name := range h.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	now := time.Now()
	if _, err := fmt.Fprintf(w, "Gonka Proxy provider health\ngenerated_at=%s\n", now.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	for _, name := range names {
		provider := h.providers[name]
		if _, err := fmt.Fprintf(w, "\nprovider %q: status=%s\n", name, providerStatus(provider, now)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  successes=%d errors=%d cooldown_transitions=%d\n", provider.Successes, provider.Errors, provider.CooldownTransitions); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  last_success=%s\n  last_error=%s category=%s status=%d\n", formatHealthTime(provider.LastSuccess), formatHealthTime(provider.LastError), valueOrNever(provider.LastErrorCategory), provider.LastErrorStatus); err != nil {
			return err
		}
		cooldown := "inactive"
		if provider.CooldownUntil.After(now) {
			cooldown = "until=" + provider.CooldownUntil.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(w, "  cooldown=%s last_transition=%s (%s)\n", cooldown, formatHealthTime(provider.LastTransition), valueOrNever(provider.LastTransitionType)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  latency_count=%d latency_avg_seconds=%s recent_latency_count=%d recent_latency_avg_seconds=%s\n", provider.LatencyCount, formatFloat(average(provider.LatencySum, provider.LatencyCount)), len(provider.RecentLatencies), formatFloat(average(sliceSum(provider.RecentLatencies), uint64(len(provider.RecentLatencies))))); err != nil {
			return err
		}
	}
	return nil
}

func providerStatus(provider *healthProviderState, now time.Time) string {
	if provider.CooldownUntil.After(now) {
		if provider.Successes == 0 {
			return "unavailable"
		}
		return "degraded"
	}
	if provider.LastSuccess.IsZero() {
		return "unavailable"
	}
	if provider.LastError.After(provider.LastSuccess) {
		return "degraded"
	}
	return "healthy"
}

func (h *healthStore) load() {
	if h.path == "" {
		return
	}
	data, err := os.ReadFile(h.path)
	if err != nil {
		if !os.IsNotExist(err) {
			h.logger.Printf("health history load error - %v", err)
		}
		return
	}
	var history healthHistoryFile
	if err := json.Unmarshal(data, &history); err != nil {
		h.logger.Printf("health history decode error - %v", err)
		return
	}
	if history.Providers != nil {
		for name, provider := range history.Providers {
			if provider == nil {
				continue
			}
			if len(provider.RecentLatencies) > healthRecentLatencySamples {
				provider.RecentLatencies = provider.RecentLatencies[len(provider.RecentLatencies)-healthRecentLatencySamples:]
			}
			h.providers[name] = provider
		}
	}
	h.events = history.Events
	h.pruneEvents(time.Now())
	h.lastPersist = history.SavedAt
}

func (h *healthStore) persistLocked(now time.Time, force bool) {
	h.pruneEvents(now)
	if h.path == "" {
		return
	}
	if !force && !h.lastPersist.IsZero() && now.Sub(h.lastPersist) < healthPersistInterval {
		return
	}
	history := healthHistoryFile{
		Version:   healthHistoryVersion,
		SavedAt:   now,
		Providers: h.providers,
		Events:    h.events,
	}
	data, err := json.MarshalIndent(history, "", "  ")
	if err == nil {
		err = writeAtomicFile(h.path, data)
	}
	if err != nil {
		if !h.persistenceWarning {
			h.logger.Printf("health history persistence error - %v", err)
			h.persistenceWarning = true
		}
		return
	}
	if h.persistenceWarning {
		h.logger.Printf("health history persistence recovered")
		h.persistenceWarning = false
	}
	h.lastPersist = now
}

func (h *healthStore) pruneEvents(now time.Time) {
	cutoff := now.Add(-healthHistoryRetention)
	first := 0
	for first < len(h.events) && h.events[first].At.Before(cutoff) {
		first++
	}
	if first > 0 {
		h.events = h.events[first:]
	}
	if len(h.events) > healthHistoryMaxEvents {
		h.events = h.events[len(h.events)-healthHistoryMaxEvents:]
	}
}

func writeAtomicFile(path string, data []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".gonka-health-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func formatHealthTime(value time.Time) string {
	if value.IsZero() {
		return "never"
	}
	return value.UTC().Format(time.RFC3339)
}

func formatFloat(value float64) string {
	return fmt.Sprintf("%.6f", value)
}

func valueOrNever(value string) string {
	if value == "" {
		return "never"
	}
	return value
}

func average(sum float64, count uint64) float64 {
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

func sliceSum(values []float64) float64 {
	var sum float64
	for _, value := range values {
		sum += value
	}
	return sum
}
