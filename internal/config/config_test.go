package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

const minimalValidConfig = `
server:
  addr: ":8080"
  read_timeout: 30s
  request_timeout: 120s
  max_body_bytes: 1048576
  shutdown_timeout: 15s
redis:
  addr: "localhost:6379"
  dial_timeout: 2s
postgres:
  dsn_env: POSTGRES_DSN
  ping_timeout: 2s
breaker:
  error_rate_threshold: 0.5
  min_requests: 20
  window: 30s
  cooldown: 10s
  cooldown_max: 5m
  half_open_probes: 3
  prober_interval: 15s
retry:
  max_attempts_per_provider: 2
  base_backoff: 200ms
  max_backoff: 5s
rate_limit:
  default_tpm: 200000
  estimate_completion_tokens: 512
observability:
  log_level: info
`

func TestLoad_ValidMinimalConfig(t *testing.T) {
	path := writeTemp(t, minimalValidConfig)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.Server.Addr != ":8080" {
		t.Errorf("Server.Addr = %q, want %q", cfg.Server.Addr, ":8080")
	}
	if cfg.Server.ReadTimeout.Std() != 30*time.Second {
		t.Errorf("Server.ReadTimeout = %v, want 30s", cfg.Server.ReadTimeout.Std())
	}
	if cfg.Redis.Addr != "localhost:6379" {
		t.Errorf("Redis.Addr = %q, want %q", cfg.Redis.Addr, "localhost:6379")
	}
	if cfg.Postgres.DSNEnv != "POSTGRES_DSN" {
		t.Errorf("Postgres.DSNEnv = %q, want %q", cfg.Postgres.DSNEnv, "POSTGRES_DSN")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("Load() expected error for missing file, got nil")
	}
}

func TestLoad_UnknownFieldsRejected(t *testing.T) {
	tests := []struct {
		name   string
		yaml   string
		errSub string
	}{
		{
			name: "unknown top-level field",
			yaml: minimalValidConfig + "\nnot_a_real_section: true\n",
		},
		{
			name: "unknown nested field",
			yaml: `
server:
  addr: ":8080"
  read_timeout: 30s
  request_timeout: 120s
  max_body_bytes: 1048576
  shutdown_timeout: 15s
  bogus_field: true
redis:
  addr: "localhost:6379"
  dial_timeout: 2s
postgres:
  dsn_env: POSTGRES_DSN
  ping_timeout: 2s
breaker:
  error_rate_threshold: 0.5
  min_requests: 20
  window: 30s
  cooldown: 10s
  cooldown_max: 5m
  half_open_probes: 3
  prober_interval: 15s
retry:
  max_attempts_per_provider: 2
  base_backoff: 200ms
  max_backoff: 5s
rate_limit:
  default_tpm: 200000
  estimate_completion_tokens: 512
observability:
  log_level: info
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, tt.yaml)
			if _, err := Load(path); err == nil {
				t.Fatal("Load() expected error for unknown field, got nil")
			}
		})
	}
}

func TestLoad_MalformedDuration(t *testing.T) {
	yaml := `
server:
  addr: ":8080"
  read_timeout: "not-a-duration"
  request_timeout: 120s
  max_body_bytes: 1048576
  shutdown_timeout: 15s
redis:
  addr: "localhost:6379"
  dial_timeout: 2s
postgres:
  dsn_env: POSTGRES_DSN
  ping_timeout: 2s
breaker:
  error_rate_threshold: 0.5
  min_requests: 20
  window: 30s
  cooldown: 10s
  cooldown_max: 5m
  half_open_probes: 3
  prober_interval: 15s
retry:
  max_attempts_per_provider: 2
  base_backoff: 200ms
  max_backoff: 5s
rate_limit:
  default_tpm: 200000
  estimate_completion_tokens: 512
observability:
  log_level: info
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for malformed duration, got nil")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("Load() error = %v, want it to mention 'duration'", err)
	}
}

func TestValidate_ReportsAllViolationsAtOnce(t *testing.T) {
	yaml := `
server:
  addr: ""
  read_timeout: 0s
  request_timeout: 0s
  max_body_bytes: 0
  shutdown_timeout: 0s
redis:
  addr: ""
  dial_timeout: 0s
postgres:
  dsn_env: ""
  ping_timeout: 0s
breaker:
  error_rate_threshold: 0
  min_requests: 0
  window: 0s
  cooldown: 0s
  cooldown_max: 0s
  half_open_probes: 0
  prober_interval: 0s
retry:
  max_attempts_per_provider: 0
  base_backoff: 0s
  max_backoff: 0s
rate_limit:
  default_tpm: 0
  estimate_completion_tokens: -1
observability:
  log_level: "not-a-level"
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error, got nil")
	}

	wantSubstrings := []string{
		"server.addr",
		"server.read_timeout",
		"server.request_timeout",
		"server.max_body_bytes",
		"server.shutdown_timeout",
		"redis.addr",
		"redis.dial_timeout",
		"postgres.dsn_env",
		"postgres.ping_timeout",
		"breaker.error_rate_threshold",
		"breaker.min_requests",
		"breaker.window",
		"breaker.cooldown",
		"breaker.half_open_probes",
		"breaker.prober_interval",
		"retry.max_attempts_per_provider",
		"retry.base_backoff",
		"rate_limit.default_tpm",
		"rate_limit.estimate_completion_tokens",
		"observability.log_level",
	}
	msg := err.Error()
	for _, want := range wantSubstrings {
		if !strings.Contains(msg, want) {
			t.Errorf("Load() error missing %q; full error: %s", want, msg)
		}
	}
}

func TestValidate_ProviderRules(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "empty name",
			yaml: minimalValidConfig + `
providers:
  - name: ""
    type: openai
    timeout: 30s
`,
			wantErr: "providers[0].name",
		},
		{
			name: "invalid type",
			yaml: minimalValidConfig + `
providers:
  - name: openai
    type: not-a-provider
    timeout: 30s
`,
			wantErr: "providers[0].type",
		},
		{
			name: "zero timeout",
			yaml: minimalValidConfig + `
providers:
  - name: openai
    type: openai
    timeout: 0s
`,
			wantErr: "providers[0].timeout",
		},
		{
			name: "valid mock provider",
			yaml: minimalValidConfig + `
providers:
  - name: mock
    type: mock
    timeout: 5s
`,
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, tt.yaml)
			_, err := Load(path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_BreakerAndRetryOrderingRules(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "cooldown_max below cooldown",
			yaml: minimalValidConfigWithout("breaker") + `
breaker:
  error_rate_threshold: 0.5
  min_requests: 20
  window: 30s
  cooldown: 10s
  cooldown_max: 5s
  half_open_probes: 3
  prober_interval: 15s
`,
			wantErr: "breaker.cooldown_max",
		},
		{
			name: "max_backoff below base_backoff",
			yaml: minimalValidConfigWithout("retry") + `
retry:
  max_attempts_per_provider: 2
  base_backoff: 1s
  max_backoff: 500ms
`,
			wantErr: "retry.max_backoff",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, tt.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load() expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// minimalValidConfigWithout returns minimalValidConfig with the named
// top-level section's own block stripped out, so a test can splice in its
// own version of just that section without ending up with two conflicting
// copies of it.
func minimalValidConfigWithout(section string) string {
	lines := strings.Split(minimalValidConfig, "\n")
	var out []string
	skipping := false
	for _, line := range lines {
		if line == section+":" {
			skipping = true
			continue
		}
		if skipping {
			if strings.HasPrefix(line, "  ") {
				continue
			}
			skipping = false
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestDuration_UnmarshalYAML(t *testing.T) {
	yaml := minimalValidConfig
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Server.ShutdownTimeout.Std() != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.Server.ShutdownTimeout.Std())
	}
}
