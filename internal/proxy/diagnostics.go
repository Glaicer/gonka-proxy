package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const diagnosticBodyLimit = 4 * 1024

type diagnosticReport struct {
	Provider    string            `json:"provider"`
	Status      string            `json:"status"`
	CheckedAt   time.Time         `json:"checked_at"`
	Category    string            `json:"category"`
	Remediation string            `json:"remediation"`
	Balance     balanceDiagnostic `json:"balance"`
	// Observed reports what real traffic did to this provider. It is read
	// from local history and adds no upstream request of its own.
	Observed observedProvider `json:"observed"`
}

type balanceDiagnostic struct {
	Status      string    `json:"status"`
	CheckedAt   time.Time `json:"checked_at,omitempty"`
	Category    string    `json:"category"`
	Remediation string    `json:"remediation"`
}

type diagnosticsResponse struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Providers   []diagnosticReport `json:"providers"`
	ModelLimits []routeLimitReport `json:"model_limits,omitempty"`
}

// routeLimitReport shows what /v1/models publishes for one route and, crucially,
// which providers serve it without declaring that dimension. A silent provider
// cannot lower the published floor on its own, so a route with gaps may in fact
// accept less than the number clients are told — which is why the two
// dimensions are tracked separately: a provider can declare a context window
// and still say nothing about how much it will generate.
type routeLimitReport struct {
	Model           string   `json:"model"`
	ContextLength   int      `json:"context_length,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	ContextUnknown  []string `json:"context_unknown_providers"`
	OutputUnknown   []string `json:"output_unknown_providers"`
	Remediation     string   `json:"remediation,omitempty"`
}

func (s *Server) serveDiagnostics(w io.Writer, ctx context.Context) error {
	observed := s.metrics.health.observe(time.Now(), healthObservationWindow)
	reports := make([]diagnosticReport, 0, len(s.providers))
	for _, selected := range s.providers {
		reports = append(reports, s.diagnoseProvider(ctx, selected, observed))
	}
	return json.NewEncoder(w).Encode(diagnosticsResponse{
		GeneratedAt: time.Now().UTC(),
		Providers:   reports,
		ModelLimits: s.routeLimitReports(),
	})
}

// routeLimitReports pairs each published floor with the providers that stayed
// silent per dimension, so an operator can see where the number is not yet
// trustworthy.
func (s *Server) routeLimitReports() []routeLimitReport {
	reports := make([]routeLimitReport, 0, len(s.modelOrder))
	for _, model := range s.modelOrder {
		routeProviders, exists := s.routes[model]
		if !exists {
			continue
		}
		report := routeLimitReport{
			Model:          model,
			ContextUnknown: []string{},
			OutputUnknown:  []string{},
		}
		if limit, ok := s.modelLimits[model]; ok {
			report.ContextLength = limit.context
			report.MaxOutputTokens = limit.output
		}
		for _, routeProvider := range routeProviders {
			declared := routeProvider.ModelLimits[model]
			name := s.metricProviderName(routeProvider)
			if declared.Context == 0 {
				report.ContextUnknown = append(report.ContextUnknown, name)
			}
			if declared.Output == 0 {
				report.OutputUnknown = append(report.OutputUnknown, name)
			}
		}
		report.Remediation = limitRemediation(model, report.ContextUnknown, "context")
		if outputRemediation := limitRemediation(model, report.OutputUnknown, "output"); outputRemediation != "" {
			if report.Remediation != "" {
				report.Remediation += "; "
			}
			report.Remediation += outputRemediation
		}
		reports = append(reports, report)
	}
	return reports
}

// limitRemediation names the silent providers of one dimension, or stays empty
// when the whole route declared it.
func limitRemediation(model string, unknown []string, dimension string) string {
	if len(unknown) == 0 {
		return ""
	}
	return fmt.Sprintf("add %s to model_limits for %s on %s so the published %s accounts for it",
		dimension, model, strings.Join(unknown, ", "), dimension)
}

func (s *Server) diagnoseProvider(ctx context.Context, selected *provider, observed map[string]observedProvider) diagnosticReport {
	checkedAt := time.Now().UTC()
	checkURL := selected.HealthCheckURL
	if checkURL == "" {
		checkURL = appendProviderPath(selected.BaseURL, "models")
	}
	status, category, remediation := s.checkDiagnosticEndpoint(ctx, checkURL, selected.APIKey)
	observedReport, ok := observed[s.metricProviderName(selected)]
	if !ok {
		observedReport = observedProvider{Status: "no-data", WindowHours: int(healthObservationWindow / time.Hour)}
	}
	report := diagnosticReport{
		Provider:    s.metricProviderName(selected),
		Status:      status,
		CheckedAt:   checkedAt,
		Category:    category,
		Remediation: remediation,
		Observed:    observedReport,
		Balance: balanceDiagnostic{
			Status:      "unavailable",
			Category:    "unsupported",
			Remediation: "no balance endpoint is configured for this provider",
		},
	}
	if selected.BalanceURL != "" {
		balanceStatus, balanceCategory, balanceRemediation := s.checkDiagnosticEndpoint(ctx, selected.BalanceURL, selected.APIKey)
		report.Balance = balanceDiagnostic{
			Status:      balanceStatusForBalance(balanceStatus),
			CheckedAt:   time.Now().UTC(),
			Category:    balanceCategory,
			Remediation: balanceRemediation,
		}
	}
	return report
}

func (s *Server) checkDiagnosticEndpoint(ctx context.Context, endpoint, apiKey string) (string, string, string) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "unavailable", "endpoint-unavailable", "update the diagnostic endpoint or provider base URL"
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := s.client.Do(request)
	if err != nil {
		if isResponseHeaderTimeout(err) {
			return "degraded", "transient-timeout", "check the endpoint and retry"
		}
		if ctx.Err() != nil {
			return "degraded", "transient-network", "check the endpoint and retry"
		}
		return "degraded", "transient-network", "check the endpoint and retry"
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, diagnosticBodyLimit))
	return classifyDiagnosticResponse(response.StatusCode, body)
}

func classifyDiagnosticResponse(statusCode int, body []byte) (string, string, string) {
	switch {
	case statusCode == http.StatusOK:
		return "healthy", "ok", "none"
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return "unavailable", "invalid-auth", "rotate or replace the provider API key"
	case statusCode == http.StatusPaymentRequired:
		return "unavailable", "insufficient-balance", "top up the provider balance"
	case statusCode == http.StatusTooManyRequests:
		if strings.Contains(strings.ToLower(string(body)), "concurr") {
			return "degraded", "concurrency-exhausted", "wait, reduce concurrency, or use another provider"
		}
		return "degraded", "rate-limit", "wait, reduce concurrency, or use another provider"
	case statusCode == http.StatusRequestTimeout || statusCode >= http.StatusInternalServerError:
		return "degraded", "transient-upstream", "check the endpoint and retry"
	case statusCode == http.StatusNotFound || statusCode == http.StatusMethodNotAllowed:
		return "unavailable", "endpoint-unavailable", "update the diagnostic endpoint or provider base URL"
	default:
		return "unavailable", "upstream-error", "inspect the provider response and endpoint configuration"
	}
}

func balanceStatusForBalance(status string) string {
	if status == "healthy" {
		return "available"
	}
	return "unavailable"
}

func appendProviderPath(baseURL, suffix string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return strings.TrimRight(baseURL, "/") + "/" + suffix
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + suffix
	parsed.RawPath = ""
	return parsed.String()
}
