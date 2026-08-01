// Package config loads and validates the gateway's YAML configuration.
//
// Only the sections a shipped phase actually consumes (server, redis,
// postgres, observability.log_level) are range-validated here. Sections for
// unbuilt phases (providers, breaker, retry, cache, rate_limit, batching)
// are still fully parsed and reject unknown fields, but their numeric
// ranges are validated by the phase that first reads them — validating a
// threshold nobody checks yet would just be dead code.
package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type ServerConfig struct {
	Addr            string   `yaml:"addr"`
	ReadTimeout     Duration `yaml:"read_timeout"`
	RequestTimeout  Duration `yaml:"request_timeout"`
	MaxBodyBytes    int64    `yaml:"max_body_bytes"`
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`
}

func (c ServerConfig) validate() []error {
	var errs []error
	if c.Addr == "" {
		errs = append(errs, errors.New("server.addr: must not be empty"))
	}
	if c.ReadTimeout.Std() <= 0 {
		errs = append(errs, errors.New("server.read_timeout: must be > 0"))
	}
	if c.RequestTimeout.Std() <= 0 {
		errs = append(errs, errors.New("server.request_timeout: must be > 0"))
	}
	if c.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("server.max_body_bytes: must be > 0"))
	}
	if c.ShutdownTimeout.Std() <= 0 {
		errs = append(errs, errors.New("server.shutdown_timeout: must be > 0"))
	}
	return errs
}

// RedisConfig is not part of the README's illustrative config snippet, but
// something has to tell the gateway where Redis lives; see
// docs/adr/0002-redis-postgres-config-sections.md.
type RedisConfig struct {
	Addr        string   `yaml:"addr"`
	PasswordEnv string   `yaml:"password_env"`
	DB          int      `yaml:"db"`
	DialTimeout Duration `yaml:"dial_timeout"`
}

func (c RedisConfig) validate() []error {
	var errs []error
	if c.Addr == "" {
		errs = append(errs, errors.New("redis.addr: must not be empty"))
	}
	if c.DialTimeout.Std() <= 0 {
		errs = append(errs, errors.New("redis.dial_timeout: must be > 0"))
	}
	return errs
}

// PostgresConfig, same rationale as RedisConfig.
type PostgresConfig struct {
	DSNEnv      string   `yaml:"dsn_env"`
	PingTimeout Duration `yaml:"ping_timeout"`
}

func (c PostgresConfig) validate() []error {
	var errs []error
	if c.DSNEnv == "" {
		errs = append(errs, errors.New("postgres.dsn_env: must not be empty"))
	}
	if c.PingTimeout.Std() <= 0 {
		errs = append(errs, errors.New("postgres.ping_timeout: must be > 0"))
	}
	return errs
}

var validProviderTypes = map[string]bool{
	"openai":    true,
	"anthropic": true,
	"mock":      true,
}

type ProviderConfig struct {
	Name      string   `yaml:"name"`
	Type      string   `yaml:"type"`
	BaseURL   string   `yaml:"base_url"`
	APIKeyEnv string   `yaml:"api_key_env"`
	Priority  int      `yaml:"priority"`
	Weight    int      `yaml:"weight"`
	Timeout   Duration `yaml:"timeout"`

	// Latency, FailureRate, and FailureStatus configure the mock provider
	// only; they're ignored for openai/anthropic. FailureStatus of 0
	// defaults to 500 in the mock provider itself.
	Latency       Duration `yaml:"latency"`
	FailureRate   float64  `yaml:"failure_rate"`
	FailureStatus int      `yaml:"failure_status"`
}

func (c ProviderConfig) validate(index int) []error {
	var errs []error
	if c.Name == "" {
		errs = append(errs, fmt.Errorf("providers[%d].name: must not be empty", index))
	}
	if !validProviderTypes[c.Type] {
		errs = append(errs, fmt.Errorf("providers[%d].type: %q is not one of openai, anthropic, mock", index, c.Type))
	}
	if c.Priority < 0 {
		errs = append(errs, fmt.Errorf("providers[%d].priority: must be >= 0", index))
	}
	if c.Weight < 0 {
		errs = append(errs, fmt.Errorf("providers[%d].weight: must be >= 0", index))
	}
	if c.Timeout.Std() <= 0 {
		errs = append(errs, fmt.Errorf("providers[%d].timeout: must be > 0", index))
	}
	if c.FailureRate < 0 || c.FailureRate > 1 {
		errs = append(errs, fmt.Errorf("providers[%d].failure_rate: must be between 0 and 1", index))
	}
	if c.Type == "openai" || c.Type == "anthropic" {
		if c.BaseURL == "" {
			errs = append(errs, fmt.Errorf("providers[%d].base_url: must not be empty for type %q", index, c.Type))
		}
		if c.APIKeyEnv == "" {
			errs = append(errs, fmt.Errorf("providers[%d].api_key_env: must not be empty for type %q", index, c.Type))
		}
	}
	return errs
}

type BreakerConfig struct {
	ErrorRateThreshold float64  `yaml:"error_rate_threshold"`
	MinRequests        int      `yaml:"min_requests"`
	Window             Duration `yaml:"window"`
	Cooldown           Duration `yaml:"cooldown"`
	CooldownMax        Duration `yaml:"cooldown_max"`
	HalfOpenProbes     int      `yaml:"half_open_probes"`
	// ProberInterval isn't in the README's example config; the background
	// prober needs a cadence and nothing else in the schema specifies one.
	// See docs/adr/0006.
	ProberInterval Duration `yaml:"prober_interval"`
}

func (c BreakerConfig) validate() []error {
	var errs []error
	if c.ErrorRateThreshold <= 0 || c.ErrorRateThreshold > 1 {
		errs = append(errs, errors.New("breaker.error_rate_threshold: must be in (0, 1]"))
	}
	if c.MinRequests < 1 {
		errs = append(errs, errors.New("breaker.min_requests: must be >= 1"))
	}
	if c.Window.Std() <= 0 {
		errs = append(errs, errors.New("breaker.window: must be > 0"))
	}
	if c.Cooldown.Std() <= 0 {
		errs = append(errs, errors.New("breaker.cooldown: must be > 0"))
	}
	if c.CooldownMax.Std() < c.Cooldown.Std() {
		errs = append(errs, errors.New("breaker.cooldown_max: must be >= breaker.cooldown"))
	}
	if c.HalfOpenProbes < 1 {
		errs = append(errs, errors.New("breaker.half_open_probes: must be >= 1"))
	}
	if c.ProberInterval.Std() <= 0 {
		errs = append(errs, errors.New("breaker.prober_interval: must be > 0"))
	}
	return errs
}

type RetryConfig struct {
	MaxAttemptsPerProvider int      `yaml:"max_attempts_per_provider"`
	BaseBackoff            Duration `yaml:"base_backoff"`
	MaxBackoff             Duration `yaml:"max_backoff"`
}

func (c RetryConfig) validate() []error {
	var errs []error
	if c.MaxAttemptsPerProvider < 1 {
		errs = append(errs, errors.New("retry.max_attempts_per_provider: must be >= 1"))
	}
	if c.BaseBackoff.Std() <= 0 {
		errs = append(errs, errors.New("retry.base_backoff: must be > 0"))
	}
	if c.MaxBackoff.Std() < c.BaseBackoff.Std() {
		errs = append(errs, errors.New("retry.max_backoff: must be >= retry.base_backoff"))
	}
	return errs
}

type ExactCacheConfig struct {
	Enabled                 bool     `yaml:"enabled"`
	TTL                     Duration `yaml:"ttl"`
	CacheNonzeroTemperature bool     `yaml:"cache_nonzero_temperature"`
}

type HNSWConfig struct {
	M              int `yaml:"m"`
	EfConstruction int `yaml:"ef_construction"`
	EfSearch       int `yaml:"ef_search"`
}

type SemanticCacheConfig struct {
	Enabled        bool       `yaml:"enabled"`
	Threshold      float64    `yaml:"threshold"`
	EmbeddingModel string     `yaml:"embedding_model"`
	HNSW           HNSWConfig `yaml:"hnsw"`
	MaxVectors     int        `yaml:"max_vectors"`
	TTL            Duration   `yaml:"ttl"`
}

type CacheConfig struct {
	Exact    ExactCacheConfig    `yaml:"exact"`
	Semantic SemanticCacheConfig `yaml:"semantic"`
}

type RateLimitConfig struct {
	DefaultTPM               int64 `yaml:"default_tpm"`
	EstimateCompletionTokens int   `yaml:"estimate_completion_tokens"`
	// FailOpen isn't in the README's example config, but the README's own
	// failure-mode table requires this behavior to be "config-switchable."
	// Defaults to false (fail closed) if omitted, the more conservative
	// reading; the README's prose leans toward true (availability over
	// cost control) as the recommended value to set explicitly.
	FailOpen bool `yaml:"fail_open"`
}

func (c RateLimitConfig) validate() []error {
	var errs []error
	if c.DefaultTPM <= 0 {
		errs = append(errs, errors.New("rate_limit.default_tpm: must be > 0"))
	}
	if c.EstimateCompletionTokens < 0 {
		errs = append(errs, errors.New("rate_limit.estimate_completion_tokens: must be >= 0"))
	}
	return errs
}

type BatchingConfig struct {
	Enabled      bool     `yaml:"enabled"`
	MaxBatchSize int      `yaml:"max_batch_size"`
	MaxWait      Duration `yaml:"max_wait"`
}

var validLogLevels = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

type ObservabilityConfig struct {
	LogLevel         string `yaml:"log_level"`
	LogRequestBodies bool   `yaml:"log_request_bodies"`
}

func (c ObservabilityConfig) validate() []error {
	var errs []error
	if !validLogLevels[c.LogLevel] {
		errs = append(errs, fmt.Errorf("observability.log_level: %q is not one of debug, info, warn, error", c.LogLevel))
	}
	return errs
}

// Config is the full gateway configuration, matching the README's shape
// plus the redis/postgres sections it omitted. Every field is decoded with
// KnownFields enabled, so a typo or a field from a future phase pasted in
// early fails loudly instead of being silently ignored.
type Config struct {
	Server         ServerConfig                 `yaml:"server"`
	Redis          RedisConfig                  `yaml:"redis"`
	Postgres       PostgresConfig               `yaml:"postgres"`
	Providers      []ProviderConfig             `yaml:"providers"`
	ModelAliases   map[string]map[string]string `yaml:"model_aliases"`
	FallbackChains map[string][]string          `yaml:"fallback_chains"`
	Breaker        BreakerConfig                `yaml:"breaker"`
	Retry          RetryConfig                  `yaml:"retry"`
	Cache          CacheConfig                  `yaml:"cache"`
	RateLimit      RateLimitConfig              `yaml:"rate_limit"`
	Batching       BatchingConfig               `yaml:"batching"`
	Observability  ObservabilityConfig          `yaml:"observability"`
}

// Load reads, strictly decodes, and validates the config file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: invalid %s: %w", path, err)
	}
	return &cfg, nil
}

// Validate collects every violation at once rather than stopping at the
// first, so a misconfigured deploy doesn't need N round trips to fix.
func (c *Config) Validate() error {
	var errs []error
	errs = append(errs, c.Server.validate()...)
	errs = append(errs, c.Redis.validate()...)
	errs = append(errs, c.Postgres.validate()...)
	for i, p := range c.Providers {
		errs = append(errs, p.validate(i)...)
	}
	errs = append(errs, c.Breaker.validate()...)
	errs = append(errs, c.Retry.validate()...)
	errs = append(errs, c.RateLimit.validate()...)
	errs = append(errs, c.Observability.validate()...)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
