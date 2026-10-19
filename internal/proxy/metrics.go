package proxy

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var metricLatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type metricsCollector struct {
	mu        sync.Mutex
	providers map[string]*providerMetrics
	health    *healthStore
}

type providerMetrics struct {
	requests          map[string]uint64
	upstreamAttempts  uint64
	upstreamResponses map[responseMetricKey]uint64
	failovers         map[string]uint64
	streamAborts      map[string]uint64
	cooldowns         uint64
	cooldownSkips     uint64
	attemptLatency    metricHistogram
	requestLatency    metricHistogram
}

type responseMetricKey struct {
	status   string
	category string
}

type metricHistogram struct {
	buckets []uint64
	sum     float64
	count   uint64
}

func newMetricsCollector(historyPath string, providerNames []string, logger Logger) *metricsCollector {
	return &metricsCollector{
		providers: make(map[string]*providerMetrics),
		health:    newHealthStore(historyPath, providerNames, logger),
	}
}

func (m *metricsCollector) provider(name string) *providerMetrics {
	provider := m.providers[name]
	if provider == nil {
		provider = &providerMetrics{
			requests:          make(map[string]uint64),
			upstreamResponses: make(map[responseMetricKey]uint64),
			failovers:         make(map[string]uint64),
			streamAborts:      make(map[string]uint64),
			attemptLatency:    metricHistogram{buckets: make([]uint64, len(metricLatencyBuckets)+1)},
			requestLatency:    metricHistogram{buckets: make([]uint64, len(metricLatencyBuckets)+1)},
		}
		m.providers[name] = provider
	}
	return provider
}

func (m *metricsCollector) recordAttempt(name string, durationSeconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	recordHistogram(&m.provider(name).attemptLatency, durationSeconds)
}

func (m *metricsCollector) recordRequestStart(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.provider(name).upstreamAttempts++
}

func (m *metricsCollector) recordRequest(name, result string, durationSeconds float64) {
	m.mu.Lock()
	provider := m.provider(name)
	provider.requests[result]++
	if result == "success" {
		recordHistogram(&provider.requestLatency, durationSeconds)
	}
	m.mu.Unlock()
	if result == "success" {
		m.health.recordSuccess(name, time.Now(), durationSeconds)
	}
}

func (m *metricsCollector) recordResponse(name string, statusCode int, category string) {
	m.mu.Lock()
	m.provider(name).upstreamResponses[responseMetricKey{
		status:   strconv.Itoa(statusCode),
		category: category,
	}]++
	m.mu.Unlock()
	if category != "success" && category != "client-error" {
		m.health.recordError(name, category, statusCode, time.Now())
	}
}

func (m *metricsCollector) recordFailover(name, category string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.provider(name).failovers[category]++
}

func (m *metricsCollector) recordCooldown(name string, until time.Time) {
	m.mu.Lock()
	m.provider(name).cooldowns++
	m.mu.Unlock()
	m.health.recordCooldown(name, until, time.Now())
}

func (m *metricsCollector) recordCooldownSkip(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.provider(name).cooldownSkips++
}

func (m *metricsCollector) recordStreamAbort(name, reason string) {
	m.mu.Lock()
	m.provider(name).streamAborts[reason]++
	m.mu.Unlock()
	m.health.recordError(name, reason, 0, time.Now())
}

func (m *metricsCollector) recordCooldownCleared(name string) {
	m.health.recordCooldownCleared(name, time.Now())
}

func (m *metricsCollector) serveHTTP(w io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := make([]string, 0, len(m.providers))
	for name := range m.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	metricTypes := []struct {
		name     string
		typeName string
	}{
		{"gonka_proxy_provider_requests_total", "counter"},
		{"gonka_proxy_provider_upstream_attempts_total", "counter"},
		{"gonka_proxy_provider_upstream_responses_total", "counter"},
		{"gonka_proxy_provider_failovers_total", "counter"},
		{"gonka_proxy_provider_stream_aborts_total", "counter"},
		{"gonka_proxy_provider_cooldowns_total", "counter"},
		{"gonka_proxy_provider_cooldown_skips_total", "counter"},
		{"gonka_proxy_provider_upstream_attempt_duration_seconds", "histogram"},
		{"gonka_proxy_provider_request_duration_seconds", "histogram"},
	}
	for _, metricType := range metricTypes {
		if _, err := fmt.Fprintf(w, "# TYPE %s %s\n", metricType.name, metricType.typeName); err != nil {
			return err
		}
	}
	for _, name := range names {
		provider := m.providers[name]
		labels := `provider="` + escapeMetricLabel(name) + `"`
		for _, result := range sortedUint64Keys(provider.requests) {
			if _, err := fmt.Fprintf(w, "gonka_proxy_provider_requests_total{%s,result=\"%s\"} %d\n", labels, result, provider.requests[result]); err != nil {
				return err
			}
		}

		if _, err := fmt.Fprintf(w, "gonka_proxy_provider_upstream_attempts_total{%s} %d\n", labels, provider.upstreamAttempts); err != nil {
			return err
		}
		responses := make([]responseMetricKey, 0, len(provider.upstreamResponses))
		for key := range provider.upstreamResponses {
			responses = append(responses, key)
		}
		sort.Slice(responses, func(i, j int) bool {
			if responses[i].status == responses[j].status {
				return responses[i].category < responses[j].category
			}
			return responses[i].status < responses[j].status
		})
		for _, key := range responses {
			if _, err := fmt.Fprintf(w, "gonka_proxy_provider_upstream_responses_total{category=\"%s\",%s,status=\"%s\"} %d\n", escapeMetricLabel(key.category), labels, escapeMetricLabel(key.status), provider.upstreamResponses[key]); err != nil {
				return err
			}
		}

		if err := writeCounterMap(w, "gonka_proxy_provider_failovers_total", labels, "category", provider.failovers); err != nil {
			return err
		}
		if err := writeCounterMap(w, "gonka_proxy_provider_stream_aborts_total", labels, "reason", provider.streamAborts); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "gonka_proxy_provider_cooldowns_total{%s} %d\ngonka_proxy_provider_cooldown_skips_total{%s} %d\n", labels, provider.cooldowns, labels, provider.cooldownSkips); err != nil {
			return err
		}
		if err := writeHistogram(w, "gonka_proxy_provider_upstream_attempt_duration_seconds", labels, provider.attemptLatency); err != nil {
			return err
		}
		if err := writeHistogram(w, "gonka_proxy_provider_request_duration_seconds", labels, provider.requestLatency); err != nil {
			return err
		}
	}
	return nil
}

func (m *metricsCollector) serveHealth(w io.Writer) error {
	return m.health.serveHTTP(w)
}

func recordHistogram(histogram *metricHistogram, value float64) {
	if value < 0 {
		value = 0
	}
	histogram.count++
	histogram.sum += value
	for index, bound := range metricLatencyBuckets {
		if value <= bound {
			histogram.buckets[index]++
			return
		}
	}
	histogram.buckets[len(histogram.buckets)-1]++
}

func writeHistogram(w io.Writer, name, labels string, histogram metricHistogram) error {
	var cumulative uint64
	for index, count := range histogram.buckets {
		cumulative += count
		bucket := "+Inf"
		if index < len(metricLatencyBuckets) {
			bucket = strconv.FormatFloat(metricLatencyBuckets[index], 'g', -1, 64)
		}
		if _, err := fmt.Fprintf(w, "%s_bucket{%s,le=\"%s\"} %d\n", name, labels, bucket, cumulative); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "%s_sum{%s} %g\n%s_count{%s} %d\n", name, labels, histogram.sum, name, labels, histogram.count)
	return err
}

func writeCounterMap(w io.Writer, name, labels, labelName string, values map[string]uint64) error {
	for _, key := range sortedUint64Keys(values) {
		if _, err := fmt.Fprintf(w, "%s{%s,%s=\"%s\"} %d\n", name, labels, labelName, escapeMetricLabel(key), values[key]); err != nil {
			return err
		}
	}
	return nil
}

func sortedUint64Keys(values map[string]uint64) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func escapeMetricLabel(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value)
}
