package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
)

func TestLoadAppliesDocumentedDefaults(t *testing.T) {
	cfg := loadConfig(t, `reasoning_effort: max
providers:
  - name: primary
    base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`)

	if cfg.Providers[0].Name != "primary" {
		t.Errorf("Provider name = %q, want %q", cfg.Providers[0].Name, "primary")
	}

	if cfg.ListenAddress != config.DefaultListenAddress {
		t.Errorf("ListenAddress = %q, want %q", cfg.ListenAddress, config.DefaultListenAddress)
	}
	if cfg.Cooldown != config.DefaultCooldown {
		t.Errorf("Cooldown = %s, want %s", cfg.Cooldown, config.DefaultCooldown)
	}
	if cfg.RecoveryWait != config.DefaultRecoveryWait {
		t.Errorf("RecoveryWait = %s, want %s", cfg.RecoveryWait, config.DefaultRecoveryWait)
	}
	if cfg.ResponseHeaderTimeout != config.DefaultResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %s, want %s", cfg.ResponseHeaderTimeout, config.DefaultResponseHeaderTimeout)
	}
	if cfg.LogLevel != config.LogLevelInfo {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, config.LogLevelInfo)
	}
}

func TestLoadParsesRecoveryWait(t *testing.T) {
	cfg := loadConfig(t, `reasoning_effort: max
recovery_wait: 25ms
providers:
  - name: primary
    base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`)

	if cfg.RecoveryWait != 25*time.Millisecond {
		t.Errorf("RecoveryWait = %s, want 25ms", cfg.RecoveryWait)
	}
}

func TestLoadParsesLogLevel(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    config.LogLevel
		wantErr string
	}{
		{name: "info", value: "INFO", want: config.LogLevelInfo},
		{name: "warn lowercase", value: "warn", want: config.LogLevelWarn},
		{name: "error", value: "ERROR", want: config.LogLevelError},
		{name: "invalid", value: "DEBUG", wantErr: "log_level must be one of INFO, WARN, or ERROR"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			contents := "log_level: " + test.value + "\nreasoning_effort: max\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n"
			if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := config.Load(configPath)
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("Load succeeded, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %q, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LogLevel != test.want {
				t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, test.want)
			}
		})
	}
}

func TestLogLevelEnabledThreshold(t *testing.T) {
	if !config.LogLevelInfo.Enabled(config.LogLevelInfo) {
		t.Error("INFO level should be enabled at INFO threshold")
	}
	if !config.LogLevelWarn.Enabled(config.LogLevelError) {
		t.Error("ERROR should be enabled at WARN threshold")
	}
	if config.LogLevelWarn.Enabled(config.LogLevelInfo) {
		t.Error("INFO should be suppressed at WARN threshold")
	}
}

func TestLoadRejectsProviderWithoutPriority(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte(`reasoning_effort: max
providers:
  - name: primary
    base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
`)
	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := config.Load(configPath)
	if err == nil {
		t.Fatal("Load succeeded without a Provider priority")
	}
	if !strings.Contains(err.Error(), "providers[0].priority is required") {
		t.Fatalf("error = %q, want missing priority detail", err)
	}
}

func TestLoadRejectsInvalidStartupConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{
			name:     "malformed YAML",
			contents: "providers:\n  - [",
			want:     "decode config",
		},
		{
			name: "invalid duration",
			contents: `reasoning_effort: max
cooldown: soon
providers:
  - name: primary
    base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`,
			want: "cooldown must be a valid duration",
		},
		{
			name: "missing API key",
			contents: `reasoning_effort: max
providers:
  - name: primary
    base_url: https://provider.example/v1
    model_alias: provider-model
    priority: 10
`,
			want: "providers[0].api_key must not be empty",
		},
		{
			name: "missing Provider name",
			contents: `reasoning_effort: max
providers:
  - base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`,
			want: "providers[0].name must not be empty",
		},
		{
			name: "duplicate Provider",
			contents: `reasoning_effort: max
providers:
  - name: primary
    base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
  - name: backup
    base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`,
			want: "providers[1] duplicates another Provider definition",
		},
		{
			name: "unusable Provider URL",
			contents: `reasoning_effort: max
providers:
  - name: primary
    base_url: ftp://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`,
			want: "providers[0].base_url must use http or https",
		},
		{
			name:     "empty Provider list",
			contents: "reasoning_effort: max\nproviders: []\n",
			want:     "providers must contain at least one Provider",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte(test.contents), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := config.Load(configPath)
			if err == nil {
				t.Fatal("Load succeeded for invalid configuration")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want substring %q", err, test.want)
			}
		})
	}
}

func TestLoadReasoningEffort(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     *string // nil means expect disabled (nil), non-nil is normalized value
		wantErr  string
	}{
		{
			name:     "absent",
			contents: "providers:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort is required",
		},
		{
			name:     "null",
			contents: "reasoning_effort: null\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     nil,
		},
		{
			name:     "tilde",
			contents: "reasoning_effort: ~\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     nil,
		},
		{
			name:     "bare null (empty value)",
			contents: "reasoning_effort:\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     nil,
		},
		{
			name:     "quoted empty",
			contents: "reasoning_effort: \"\"\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort must be one of none, low, medium, high, xhigh, max, null",
		},
		{
			name:     "quoted empty single",
			contents: "reasoning_effort: ''\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort must be one of none, low, medium, high, xhigh, max, null",
		},
		{
			name:     "quoted null strict",
			contents: "reasoning_effort: \"null\"\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort must be one of none, low, medium, high, xhigh, max, null",
		},
		{
			name:     "quoted NULL strict",
			contents: "reasoning_effort: 'NULL'\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort must be one of none, low, medium, high, xhigh, max, null",
		},
		{
			name:     "trimmed max",
			contents: "reasoning_effort: \" MAX \"\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     stringPtr("max"),
		},
		{
			name:     "MAX uppercase",
			contents: "reasoning_effort: MAX\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     stringPtr("max"),
		},
		{
			name:     "low normalized",
			contents: "reasoning_effort: Low\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     stringPtr("low"),
		},
		{
			name:     "high trimmed",
			contents: "reasoning_effort: \" high \"\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     stringPtr("high"),
		},
		{
			name:     "none forwarded",
			contents: "reasoning_effort: none\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     stringPtr("none"),
		},
		{
			name:     "NONE uppercase",
			contents: "reasoning_effort: NONE\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     stringPtr("none"),
		},
		{
			name:     "medium accepted",
			contents: "reasoning_effort: medium\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     stringPtr("medium"),
		},
		{
			name:     "xhigh accepted",
			contents: "reasoning_effort: xhigh\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     stringPtr("xhigh"),
		},
		{
			name:     "XHIGH trimmed and normalized",
			contents: "reasoning_effort: \" XHigh \"\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			want:     stringPtr("xhigh"),
		},
		{
			name:     "minimal invalid",
			contents: "reasoning_effort: minimal\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort must be one of none, low, medium, high, xhigh, max, null",
		},
		{
			name:     "numeric invalid",
			contents: "reasoning_effort: 123\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort must be one of none, low, medium, high, xhigh, max, null",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte(test.contents), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := config.Load(configPath)
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("Load succeeded, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %q, want substring %q", err, test.wantErr)
				}
				if strings.Contains(test.wantErr, "null") && !strings.Contains(err.Error(), "null") {
					t.Fatalf("error = %q, want to contain %q as per spec message", err, "null")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if test.want == nil {
				if cfg.ReasoningEffort != nil {
					t.Fatalf("ReasoningEffort = %q, want nil (disabled)", *cfg.ReasoningEffort)
				}
			} else {
				if cfg.ReasoningEffort == nil {
					t.Fatalf("ReasoningEffort = nil, want %q", *test.want)
				}
				if string(*cfg.ReasoningEffort) != *test.want {
					t.Errorf("ReasoningEffort = %q, want %q", *cfg.ReasoningEffort, *test.want)
				}
			}
		})
	}
}

func TestLoadProviderReasoningEffort(t *testing.T) {
	providerTemplate := func(effortLine string) string {
		return "reasoning_effort: max\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n" + effortLine
	}
	tests := []struct {
		name       string
		effortLine string
		want       *string // nil + !wantStrip means inherit (nil override)
		wantStrip  bool
		wantErr    string
	}{
		{
			name:       "absent inherits global",
			effortLine: "",
			want:       nil,
		},
		{
			name:       "null strips for this provider",
			effortLine: "    reasoning_effort: null\n",
			wantStrip:  true,
		},
		{
			name:       "tilde strips for this provider",
			effortLine: "    reasoning_effort: ~\n",
			wantStrip:  true,
		},
		{
			name:       "bare null strips",
			effortLine: "    reasoning_effort:\n",
			wantStrip:  true,
		},
		{
			name:       "value overrides global",
			effortLine: "    reasoning_effort: high\n",
			want:       stringPtr("high"),
		},
		{
			name:       "uppercase normalized",
			effortLine: "    reasoning_effort: HIGH\n",
			want:       stringPtr("high"),
		},
		{
			name:       "trimmed value normalized",
			effortLine: "    reasoning_effort: \" low \"\n",
			want:       stringPtr("low"),
		},
		{
			name:       "none forwarded",
			effortLine: "    reasoning_effort: none\n",
			want:       stringPtr("none"),
		},
		{
			name:       "medium accepted",
			effortLine: "    reasoning_effort: Medium\n",
			want:       stringPtr("medium"),
		},
		{
			name:       "xhigh accepted",
			effortLine: "    reasoning_effort: \" XHIGH \"\n",
			want:       stringPtr("xhigh"),
		},
		{
			name:       "quoted null is an error",
			effortLine: "    reasoning_effort: \"null\"\n",
			wantErr:    "providers[0].reasoning_effort must be one of none, low, medium, high, xhigh, max, null",
		},
		{
			name:       "quoted empty is an error",
			effortLine: "    reasoning_effort: \"\"\n",
			wantErr:    "providers[0].reasoning_effort must be one of none, low, medium, high, xhigh, max, null",
		},
		{
			name:       "minimal is an error",
			effortLine: "    reasoning_effort: minimal\n",
			wantErr:    "providers[0].reasoning_effort must be one of none, low, medium, high, xhigh, max, null",
		},
		{
			name:       "numeric is an error",
			effortLine: "    reasoning_effort: 123\n",
			wantErr:    "providers[0].reasoning_effort must be one of none, low, medium, high, xhigh, max, null",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte(providerTemplate(test.effortLine)), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := config.Load(configPath)
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("Load succeeded, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %q, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Providers) != 1 {
				t.Fatalf("providers = %d, want 1", len(cfg.Providers))
			}
			provider := cfg.Providers[0]
			if provider.StripReasoningEffort != test.wantStrip {
				t.Fatalf("StripReasoningEffort = %v, want %v", provider.StripReasoningEffort, test.wantStrip)
			}
			if test.want == nil {
				if provider.ReasoningEffort != nil {
					t.Fatalf("ReasoningEffort = %q, want nil (inherit)", *provider.ReasoningEffort)
				}
				return
			}
			if provider.ReasoningEffort == nil {
				t.Fatalf("ReasoningEffort = nil, want %q", *test.want)
			}
			if string(*provider.ReasoningEffort) != *test.want {
				t.Errorf("ReasoningEffort = %q, want %q", *provider.ReasoningEffort, *test.want)
			}
		})
	}
}

func TestLoadProviderReasoningEffortErrorIndex(t *testing.T) {
	contents := "reasoning_effort: max\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n  - name: backup\n    base_url: https://backup.example/v1\n    api_key: backup-secret\n    model_alias: backup-model\n    priority: 5\n    reasoning_effort: extreme\n"
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := config.Load(configPath)
	if err == nil {
		t.Fatal("Load succeeded, want error")
	}
	if !strings.Contains(err.Error(), "providers[1].reasoning_effort must be one of none, low, medium, high, xhigh, max, null") {
		t.Fatalf("error = %q, want providers[1] message", err)
	}
}

func TestValidateRejectsConflictingProviderReasoningEffort(t *testing.T) {
	effort := config.ReasoningEffortHigh
	cfg := config.Config{
		ListenAddress:         config.DefaultListenAddress,
		Cooldown:              config.DefaultCooldown,
		RecoveryWait:          config.DefaultRecoveryWait,
		ResponseHeaderTimeout: config.DefaultResponseHeaderTimeout,
		ReasoningEffort:       nil,
		Providers: []config.Provider{
			{Name: "primary", BaseURL: "https://provider.example/v1", APIKey: "provider-secret", ModelAlias: "provider-model", Priority: 10, ReasoningEffort: &effort, StripReasoningEffort: true},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate with both ReasoningEffort and StripReasoningEffort should fail")
	}
	if !strings.Contains(err.Error(), "providers[0]") {
		t.Fatalf("error = %q, want providers[0] prefix", err)
	}

	invalidEffort := config.ReasoningEffort("extreme")
	cfg.Providers[0] = config.Provider{Name: "primary", BaseURL: "https://provider.example/v1", APIKey: "provider-secret", ModelAlias: "provider-model", Priority: 10, ReasoningEffort: &invalidEffort}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("Validate with invalid provider ReasoningEffort should fail")
	}
	if !strings.Contains(err.Error(), "providers[0].reasoning_effort must be one of none, low, medium, high, xhigh, max, null") {
		t.Fatalf("error = %q, want enum message", err)
	}
}

func TestValidateReasoningEffortCaseInsensitive(t *testing.T) {
	upper := config.ReasoningEffort("MAX")
	cfg := config.Config{
		ListenAddress:         config.DefaultListenAddress,
		Cooldown:              config.DefaultCooldown,
		RecoveryWait:          config.DefaultRecoveryWait,
		ResponseHeaderTimeout: config.DefaultResponseHeaderTimeout,
		ReasoningEffort:       &upper,
		Providers: []config.Provider{
			{Name: "primary", BaseURL: "https://provider.example/v1", APIKey: "provider-secret", ModelAlias: "provider-model", Priority: 10},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with uppercase MAX should pass, got %v", err)
	}
	invalid := config.ReasoningEffort("extreme")
	cfg.ReasoningEffort = &invalid
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate with extreme should fail")
	} else if !strings.Contains(err.Error(), "null") {
		t.Fatalf("error = %q, want to contain null", err)
	}
}

func stringPtr(s string) *string { return &s }

func loadConfig(t *testing.T, contents string) config.Config {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}
