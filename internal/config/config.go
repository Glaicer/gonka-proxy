package config

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultListenAddress         = "0.0.0.0:8080"
	DefaultCooldown              = 120 * time.Second
	DefaultRecoveryWait          = 30 * time.Second
	DefaultResponseHeaderTimeout = 60 * time.Second
	DefaultLogLevel              = "INFO"
)

// LogLevel is a configurable minimum severity threshold. Lines at or above
// this threshold are emitted. Valid values are INFO, WARN, and ERROR.
type LogLevel string

const (
	LogLevelInfo  LogLevel = "INFO"
	LogLevelWarn  LogLevel = "WARN"
	LogLevelError LogLevel = "ERROR"
)

func (l LogLevel) severity() int {
	switch l {
	case LogLevelError:
		return 30
	case LogLevelWarn:
		return 20
	default:
		return 10
	}
}

// Enabled reports whether a line at the given level passes this threshold.
func (l LogLevel) Enabled(level LogLevel) bool {
	return level.severity() >= l.severity()
}

// Config is the validated runtime configuration for the proxy.
type Config struct {
	ListenAddress         string
	Cooldown              time.Duration
	RecoveryWait          time.Duration
	ResponseHeaderTimeout time.Duration
	LogLevel              LogLevel
	Providers             []Provider
}

// Provider is one OpenAI-compatible inference endpoint in the routing pool.
type Provider struct {
	Name       string
	BaseURL    string
	APIKey     string
	ModelAlias string
	Priority   int
}

type rawConfig struct {
	Server                rawServer     `yaml:"server"`
	Cooldown              string        `yaml:"cooldown"`
	RecoveryWait          string        `yaml:"recovery_wait"`
	ResponseHeaderTimeout string        `yaml:"response_header_timeout"`
	LogLevel              string        `yaml:"log_level"`
	Providers             []rawProvider `yaml:"providers"`
}

type rawServer struct {
	ListenAddress string `yaml:"listen_address"`
}

type rawProvider struct {
	Name       string `yaml:"name"`
	BaseURL    string `yaml:"base_url"`
	APIKey     string `yaml:"api_key"`
	ModelAlias string `yaml:"model_alias"`
	Priority   *int   `yaml:"priority"`
}

// Load reads, defaults, normalizes, and validates one YAML configuration file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var raw rawConfig
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	var extraDocument any
	if err := decoder.Decode(&extraDocument); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("decode config %q: multiple YAML documents are not supported", path)
		}
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}

	cfg := Config{
		ListenAddress:         strings.TrimSpace(raw.Server.ListenAddress),
		Cooldown:              0,
		RecoveryWait:          0,
		ResponseHeaderTimeout: 0,
		LogLevel:              LogLevelInfo,
		Providers:             make([]Provider, 0, len(raw.Providers)),
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = DefaultListenAddress
	}

	if cfg.Cooldown, err = parseDuration("cooldown", raw.Cooldown, DefaultCooldown); err != nil {
		return Config{}, err
	}
	if cfg.RecoveryWait, err = parseDuration("recovery_wait", raw.RecoveryWait, DefaultRecoveryWait); err != nil {
		return Config{}, err
	}
	if cfg.ResponseHeaderTimeout, err = parseDuration("response_header_timeout", raw.ResponseHeaderTimeout, DefaultResponseHeaderTimeout); err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = parseLogLevel(raw.LogLevel); err != nil {
		return Config{}, err
	}

	for index, rawProvider := range raw.Providers {
		provider, err := normalizeProvider(index, rawProvider)
		if err != nil {
			return Config{}, err
		}
		cfg.Providers = append(cfg.Providers, provider)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate ensures a Config can be used to start the proxy.
func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddress) == "" {
		return fmt.Errorf("server.listen_address must not be empty")
	}
	if c.Cooldown <= 0 {
		return fmt.Errorf("cooldown must be greater than zero")
	}
	if c.RecoveryWait <= 0 {
		return fmt.Errorf("recovery_wait must be greater than zero")
	}
	if c.ResponseHeaderTimeout <= 0 {
		return fmt.Errorf("response_header_timeout must be greater than zero")
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("providers must contain at least one Provider")
	}

	seen := make(map[providerIdentity]struct{}, len(c.Providers))
	for index, provider := range c.Providers {
		if strings.TrimSpace(provider.Name) == "" {
			return fmt.Errorf("providers[%d].name must not be empty", index)
		}
		if strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("providers[%d].base_url must not be empty", index)
		}
		if strings.TrimSpace(provider.APIKey) == "" {
			return fmt.Errorf("providers[%d].api_key must not be empty", index)
		}
		if strings.TrimSpace(provider.ModelAlias) == "" {
			return fmt.Errorf("providers[%d].model_alias must not be empty", index)
		}
		if _, err := parseProviderBaseURL(index, provider.BaseURL); err != nil {
			return err
		}
		definitionKey := providerIdentity{
			baseURL:    provider.BaseURL,
			apiKey:     provider.APIKey,
			modelAlias: provider.ModelAlias,
			priority:   provider.Priority,
		}
		if _, exists := seen[definitionKey]; exists {
			return fmt.Errorf("providers[%d] duplicates another Provider definition", index)
		}
		seen[definitionKey] = struct{}{}
	}
	return nil
}

type providerIdentity struct {
	baseURL    string
	apiKey     string
	modelAlias string
	priority   int
}

func parseDuration(field, value string, defaultValue time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultValue, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration such as 60s: %w", field, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", field)
	}
	return duration, nil
}

func parseLogLevel(value string) (LogLevel, error) {
	switch LogLevel(strings.ToUpper(strings.TrimSpace(value))) {
	case "":
		return LogLevelInfo, nil
	case LogLevelInfo:
		return LogLevelInfo, nil
	case LogLevelWarn:
		return LogLevelWarn, nil
	case LogLevelError:
		return LogLevelError, nil
	default:
		return "", fmt.Errorf("log_level must be one of INFO, WARN, or ERROR")
	}
}

func normalizeProvider(index int, raw rawProvider) (Provider, error) {
	if raw.Priority == nil {
		return Provider{}, fmt.Errorf("providers[%d].priority is required", index)
	}
	parsed, err := parseProviderBaseURL(index, raw.BaseURL)
	if err != nil {
		return Provider{}, err
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	normalizedURL := strings.TrimRight(parsed.String(), "/")

	provider := Provider{
		Name:       strings.TrimSpace(raw.Name),
		BaseURL:    normalizedURL,
		APIKey:     strings.TrimSpace(raw.APIKey),
		ModelAlias: strings.TrimSpace(raw.ModelAlias),
		Priority:   *raw.Priority,
	}
	return provider, nil
}

func parseProviderBaseURL(index int, value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("providers[%d].base_url must be an absolute HTTP(S) URL", index)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("providers[%d].base_url must use http or https", index)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("providers[%d].base_url must not contain credentials, query parameters, or fragments", index)
	}
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/chat/completions") {
		return nil, fmt.Errorf("providers[%d].base_url must be an API root, not a chat-completions URL", index)
	}
	return parsed, nil
}
