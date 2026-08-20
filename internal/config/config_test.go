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
	cfg := loadConfig(t, `providers:
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
	cfg := loadConfig(t, `recovery_wait: 25ms
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
			contents := "log_level: " + test.value + "\nproviders:\n  - name: primary\n    base_url: https://provider.example/v1\n    api_key: provider-secret\n    model_alias: provider-model\n    priority: 10\n"
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
	contents := []byte(`providers:
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
			contents: `cooldown: soon
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
			contents: `providers:
  - name: primary
    base_url: https://provider.example/v1
    model_alias: provider-model
    priority: 10
`,
			want: "providers[0].api_key must not be empty",
		},
		{
			name: "missing Provider name",
			contents: `providers:
  - base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`,
			want: "providers[0].name must not be empty",
		},
		{
			name: "duplicate Provider",
			contents: `providers:
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
			contents: `providers:
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
			contents: "providers: []\n",
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
