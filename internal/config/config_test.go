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
		name    string
		contents string
		want    *string // nil means expect disabled (nil), non-nil is normalized value
		wantErr string
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
			wantErr:  "reasoning_effort must be one of low, high, max, null",
		},
		{
			name:     "quoted empty single",
			contents: "reasoning_effort: ''\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort must be one of low, high, max, null",
		},
		{
			name:     "quoted null strict",
			contents: "reasoning_effort: \"null\"\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort must be one of low, high, max, null",
		},
		{
			name:     "quoted NULL strict",
			contents: "reasoning_effort: 'NULL'\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort must be one of low, high, max, null",
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
			name:     "medium invalid",
			contents: "reasoning_effort: medium\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort must be one of low, high, max, null",
		},
		{
			name:     "numeric invalid",
			contents: "reasoning_effort: 123\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n",
			wantErr:  "reasoning_effort must be one of low, high, max, null",
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
	invalid := config.ReasoningEffort("medium")
	cfg.ReasoningEffort = &invalid
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate with medium should fail")
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
