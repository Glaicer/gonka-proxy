package proxy

import (
	"context"
	"encoding/json"
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
}

func (s *Server) serveDiagnostics(w io.Writer, ctx context.Context) error {
	reports := make([]diagnosticReport, 0, len(s.providers))
	for _, selected := range s.providers {
		reports = append(reports, s.diagnoseProvider(ctx, selected))
	}
	return json.NewEncoder(w).Encode(diagnosticsResponse{
		GeneratedAt: time.Now().UTC(),
		Providers:   reports,
	})
}

func (s *Server) diagnoseProvider(ctx context.Context, selected *provider) diagnosticReport {
	checkedAt := time.Now().UTC()
	checkURL := selected.HealthCheckURL
	if checkURL == "" {
		checkURL = appendProviderPath(selected.BaseURL, "models")
	}
	status, category, remediation := s.checkDiagnosticEndpoint(ctx, checkURL, selected.APIKey)
	report := diagnosticReport{
		Provider:    s.metricProviderName(selected),
		Status:      status,
		CheckedAt:   checkedAt,
		Category:    category,
		Remediation: remediation,
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
