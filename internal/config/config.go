package config

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultListenAddress         = "0.0.0.0:8080"
	DefaultCooldown              = 120 * time.Second
	DefaultRecoveryWait          = 30 * time.Second
	DefaultResponseHeaderTimeout = 30 * time.Second
	DefaultLogLevel              = "WARN"
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

// ReasoningEffort is the provider reasoning intensity. Nil means null/disabled.
type ReasoningEffort string

const (
	ReasoningEffortNone   ReasoningEffort = "none"
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
	ReasoningEffortXHigh  ReasoningEffort = "xhigh"
	ReasoningEffortMax    ReasoningEffort = "max"
)

const reasoningEffortErrorMsg = "reasoning_effort must be one of none, low, medium, high, xhigh, max, null"

// IsValid reports whether the effort is one of the allowed non-null values.
// Comparison is case-insensitive and trims surrounding whitespace so that
// programmatic Config values like "MAX" or " Max " are accepted.
func (r ReasoningEffort) IsValid() bool {
	switch r.Normalize() {
	case ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium,
		ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax:
		return true
	default:
		return false
	}
}

// Normalize lowercases and trims the effort so programmatic Config values
// like "MAX" behave like loaded YAML values.
func (r ReasoningEffort) Normalize() ReasoningEffort {
	return ReasoningEffort(strings.ToLower(strings.TrimSpace(string(r))))
}

// parseReasoningEffort normalizes and validates a reasoning_effort string value.
func parseReasoningEffort(s string) (*ReasoningEffort, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, fmt.Errorf(reasoningEffortErrorMsg)
	}
	lowered := ReasoningEffort(trimmed).Normalize()
	if !lowered.IsValid() {
		return nil, fmt.Errorf(reasoningEffortErrorMsg)
	}
	v := lowered
	return &v, nil
}

// providerReasoningEffortError scopes the shared enum message to one provider.
func providerReasoningEffortError(index int) error {
	return fmt.Errorf("providers[%d].%s", index, reasoningEffortErrorMsg)
}

// Config is the validated runtime configuration for the proxy.
type Config struct {
	ListenAddress         string
	Cooldown              time.Duration
	RecoveryWait          time.Duration
	ResponseHeaderTimeout time.Duration
	LogLevel              LogLevel
	ReasoningEffort       *ReasoningEffort
	FailoverOn404         bool
	Providers             []Provider
	ModelRoutes           map[string]ModelRoute
	ModelRouteOrder       []string
}

// ModelRoute maps a Virtual Model to an optional ordered list of Provider
// names. An empty Providers list means use the global Provider priority order.
type ModelRoute struct {
	Providers []string
}

// Provider is one OpenAI-compatible inference endpoint in the routing pool.
// A provider-level reasoning_effort overrides the global value; an explicit
// null strips the field for this provider. Leave both zero to inherit global.
type Provider struct {
	Name         string
	BaseURL      string
	APIKey       string
	ModelAlias   string
	ModelAliases map[string]string
	Priority     int

	ReasoningEffort      *ReasoningEffort
	StripReasoningEffort bool
}

type rawConfig struct {
	Server                rawServer                `yaml:"server"`
	Cooldown              string                   `yaml:"cooldown"`
	RecoveryWait          string                   `yaml:"recovery_wait"`
	ResponseHeaderTimeout string                   `yaml:"response_header_timeout"`
	LogLevel              string                   `yaml:"log_level"`
	ReasoningEffort       *string                  `yaml:"reasoning_effort"` // present for KnownFields; value sourced from rawMap to distinguish null vs absent
	FailoverOn404         bool                     `yaml:"failover_on_404"`
	Providers             []rawProvider            `yaml:"providers"`
	ModelRoutes           map[string]rawModelRoute `yaml:"model_routes"`
}

type rawServer struct {
	ListenAddress string `yaml:"listen_address"`
}

type rawProvider struct {
	Name         string            `yaml:"name"`
	BaseURL      string            `yaml:"base_url"`
	APIKey       string            `yaml:"api_key"`
	ModelAlias   string            `yaml:"model_alias"`
	ModelAliases map[string]string `yaml:"model_aliases"`
	Priority     *int              `yaml:"priority"`
	// ReasoningEffort keeps the raw node to distinguish absent (inherit
	// global, zero Node) from explicit null (strip for this provider).
	ReasoningEffort yaml.Node `yaml:"reasoning_effort"`
}

type rawModelRoute struct {
	Providers []string `yaml:"providers"`
}

// Load reads, defaults, normalizes, and validates one YAML configuration file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	// Inspect reasoning_effort presence and value before strict struct decoding,
	// to distinguish absent vs explicit null (both decode as nil into *string).
	var rawMap map[string]yaml.Node
	if err := yaml.Unmarshal(data, &rawMap); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	modelRouteOrder := parseModelRouteOrder(data)
	node, ok := rawMap["reasoning_effort"]
	if !ok {
		return Config{}, fmt.Errorf("reasoning_effort is required")
	}
	var parsedReasoningEffort *ReasoningEffort
	if node.Tag == "!!null" {
		parsedReasoningEffort = nil
	} else {
		if node.Tag != "!!str" {
			return Config{}, fmt.Errorf(reasoningEffortErrorMsg)
		}
		parsed, err := parseReasoningEffort(node.Value)
		if err != nil {
			return Config{}, err
		}
		parsedReasoningEffort = parsed
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
		LogLevel:              LogLevel(DefaultLogLevel),
		ReasoningEffort:       parsedReasoningEffort,
		FailoverOn404:         raw.FailoverOn404,
		Providers:             make([]Provider, 0, len(raw.Providers)),
		ModelRoutes:           make(map[string]ModelRoute, len(raw.ModelRoutes)),
		ModelRouteOrder:       modelRouteOrder,
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
	for name, rawRoute := range raw.ModelRoutes {
		providers := make([]string, 0, len(rawRoute.Providers))
		for _, providerName := range rawRoute.Providers {
			providers = append(providers, strings.TrimSpace(providerName))
		}
		cfg.ModelRoutes[strings.TrimSpace(name)] = ModelRoute{Providers: providers}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseModelRouteOrder(data []byte) []string {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil || len(document.Content) == 0 {
		return nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value != "model_routes" {
			continue
		}
		routes := root.Content[index+1]
		if routes.Kind != yaml.MappingNode {
			return nil
		}
		order := make([]string, 0, len(routes.Content)/2)
		for routeIndex := 0; routeIndex+1 < len(routes.Content); routeIndex += 2 {
			name := strings.TrimSpace(routes.Content[routeIndex].Value)
			if name != "" {
				order = append(order, name)
			}
		}
		return order
	}
	return nil
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
	if err := c.validateModelRoutes(); err != nil {
		return err
	}
	if c.ReasoningEffort != nil && !c.ReasoningEffort.IsValid() {
		return fmt.Errorf(reasoningEffortErrorMsg)
	}

	seenNames := make(map[string]int, len(c.Providers))
	seen := make(map[providerIdentity]struct{}, len(c.Providers))
	for index, provider := range c.Providers {
		name := strings.TrimSpace(provider.Name)
		if name == "" {
			return fmt.Errorf("providers[%d].name must not be empty", index)
		}
		if previousIndex, exists := seenNames[name]; exists {
			return fmt.Errorf("providers[%d].name duplicates providers[%d].name", index, previousIndex)
		}
		seenNames[name] = index
		if strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("providers[%d].base_url must not be empty", index)
		}
		if strings.TrimSpace(provider.APIKey) == "" {
			return fmt.Errorf("providers[%d].api_key must not be empty", index)
		}
		if strings.TrimSpace(provider.ModelAlias) == "" && len(provider.ModelAliases) == 0 {
			return fmt.Errorf("providers[%d].model_alias must not be empty", index)
		}
		for model, alias := range provider.ModelAliases {
			if strings.TrimSpace(model) == "" || strings.TrimSpace(alias) == "" {
				return fmt.Errorf("providers[%d].model_aliases must not contain empty model names or aliases", index)
			}
		}
		if provider.ReasoningEffort != nil && !provider.ReasoningEffort.IsValid() {
			return providerReasoningEffortError(index)
		}
		if provider.StripReasoningEffort && provider.ReasoningEffort != nil {
			return fmt.Errorf("providers[%d].reasoning_effort must be either null or an effort value", index)
		}
		if _, err := parseProviderBaseURL(index, provider.BaseURL); err != nil {
			return err
		}
		definitionKey := providerIdentity{
			baseURL:      provider.BaseURL,
			apiKey:       provider.APIKey,
			modelAlias:   provider.ModelAlias,
			modelAliases: modelAliasesIdentity(provider.ModelAliases),
			priority:     provider.Priority,
		}
		if _, exists := seen[definitionKey]; exists {
			return fmt.Errorf("providers[%d] duplicates another Provider definition", index)
		}
		seen[definitionKey] = struct{}{}
	}
	return nil
}

type providerIdentity struct {
	baseURL      string
	apiKey       string
	modelAlias   string
	modelAliases string
	priority     int
}

func (c Config) validateModelRoutes() error {
	if len(c.ModelRoutes) == 0 {
		if len(c.ModelRouteOrder) > 0 {
			return fmt.Errorf("model_route_order must be empty when model_routes is empty")
		}
		for _, provider := range c.Providers {
			if strings.TrimSpace(provider.ModelAlias) != "" {
				return nil
			}
		}
		return fmt.Errorf("model_routes must contain at least one route when providers use model_aliases")
	}

	providersByName := make(map[string]Provider, len(c.Providers))
	for _, provider := range c.Providers {
		providersByName[strings.TrimSpace(provider.Name)] = provider
	}

	if len(c.ModelRouteOrder) != len(c.ModelRoutes) {
		return fmt.Errorf("model_route_order must list every model route exactly once")
	}
	orderedRoutes := make(map[string]struct{}, len(c.ModelRouteOrder))
	for index, rawRouteName := range c.ModelRouteOrder {
		routeName := strings.TrimSpace(rawRouteName)
		if routeName == "" {
			return fmt.Errorf("model_route_order[%d] must not be empty", index)
		}
		if _, exists := c.ModelRoutes[routeName]; !exists {
			return fmt.Errorf("model_route_order[%d] references unknown model route %q", index, routeName)
		}
		if _, exists := orderedRoutes[routeName]; exists {
			return fmt.Errorf("model_route_order contains duplicate model route %q", routeName)
		}
		orderedRoutes[routeName] = struct{}{}
	}

	for rawRouteName, route := range c.ModelRoutes {
		routeName := strings.TrimSpace(rawRouteName)
		if routeName == "" {
			return fmt.Errorf("model_routes contains an empty route name")
		}
		seen := make(map[string]struct{}, len(route.Providers))
		usable := 0
		for index, providerName := range route.Providers {
			providerName = strings.TrimSpace(providerName)
			if providerName == "" {
				return fmt.Errorf("model_routes[%q].providers[%d] must not be empty", routeName, index)
			}
			if _, exists := seen[providerName]; exists {
				return fmt.Errorf("model_routes[%q].providers contains duplicate Provider %q", routeName, providerName)
			}
			seen[providerName] = struct{}{}
			provider, exists := providersByName[providerName]
			if !exists {
				return fmt.Errorf("model_routes[%q] references unknown Provider %q", routeName, providerName)
			}
			if _, ok := provider.ModelAliasFor(routeName); ok {
				usable++
			}
		}
		if len(route.Providers) > 0 && usable == 0 {
			return fmt.Errorf("model_routes[%q] has no Providers with a model alias", routeName)
		}
		if len(route.Providers) == 0 {
			for _, provider := range c.Providers {
				if _, ok := provider.ModelAliasFor(routeName); ok {
					usable++
				}
			}
			if usable == 0 {
				return fmt.Errorf("model_routes[%q] has no Providers with a model alias", routeName)
			}
		}
	}
	return nil
}

// ModelAliasFor resolves the upstream alias for a Virtual Model. A configured
// model_aliases map takes precedence over the legacy scalar model_alias.
func (p Provider) ModelAliasFor(model string) (string, bool) {
	if len(p.ModelAliases) > 0 {
		alias, ok := p.ModelAliases[model]
		return strings.TrimSpace(alias), ok && strings.TrimSpace(alias) != ""
	}
	if model == "gonka" && strings.TrimSpace(p.ModelAlias) != "" {
		return strings.TrimSpace(p.ModelAlias), true
	}
	return "", false
}

func modelAliasesIdentity(aliases map[string]string) string {
	if len(aliases) == 0 {
		return ""
	}
	keys := make([]string, 0, len(aliases))
	for key := range aliases {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(aliases[key])
		builder.WriteByte(';')
	}
	return builder.String()
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
		return LogLevel(DefaultLogLevel), nil
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

	var effortOverride *ReasoningEffort
	var stripEffort bool
	if node := raw.ReasoningEffort; !node.IsZero() {
		switch node.Tag {
		case "!!null":
			stripEffort = true
		case "!!str":
			override, err := parseReasoningEffort(node.Value)
			if err != nil {
				return Provider{}, providerReasoningEffortError(index)
			}
			effortOverride = override
		default:
			return Provider{}, providerReasoningEffortError(index)
		}
	}

	provider := Provider{
		Name:         strings.TrimSpace(raw.Name),
		BaseURL:      normalizedURL,
		APIKey:       strings.TrimSpace(raw.APIKey),
		ModelAlias:   strings.TrimSpace(raw.ModelAlias),
		ModelAliases: normalizeModelAliases(raw.ModelAliases),
		Priority:     *raw.Priority,

		ReasoningEffort:      effortOverride,
		StripReasoningEffort: stripEffort,
	}
	return provider, nil
}

func normalizeModelAliases(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	normalized := make(map[string]string, len(raw))
	for model, alias := range raw {
		normalized[strings.TrimSpace(model)] = strings.TrimSpace(alias)
	}
	return normalized
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
