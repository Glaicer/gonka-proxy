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
  - base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`)

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
}

func TestLoadParsesRecoveryWait(t *testing.T) {
	cfg := loadConfig(t, `recovery_wait: 25ms
providers:
  - base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`)

	if cfg.RecoveryWait != 25*time.Millisecond {
		t.Errorf("RecoveryWait = %s, want 25ms", cfg.RecoveryWait)
	}
}

func TestLoadRejectsProviderWithoutPriority(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte(`providers:
  - base_url: https://provider.example/v1
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
  - base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`,
			want: "cooldown must be a valid duration",
		},
		{
			name: "missing API key",
			contents: `providers:
  - base_url: https://provider.example/v1
    model_alias: provider-model
    priority: 10
`,
			want: "providers[0].api_key must not be empty",
		},
		{
			name: "duplicate Provider",
			contents: `providers:
  - base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
  - base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`,
			want: "providers[1] duplicates another Provider definition",
		},
		{
			name: "unusable Provider URL",
			contents: `providers:
  - base_url: ftp://provider.example/v1
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
