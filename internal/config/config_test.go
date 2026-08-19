package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
