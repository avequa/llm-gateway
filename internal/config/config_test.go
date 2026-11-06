package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
server:
  addr: 0.0.0.0:4000
upstreams:
  ollama:
    base_url: http://localhost:11434/v1/
  cloud:
    base_url: https://api.example.com/v1
    api_key_env: CLOUD_KEY
    timeout: 30s
    max_concurrent: 8
models:
  small:
    targets:
      - upstream: ollama
        model: qwen3:4b
  main:
    targets:
      - upstream: ollama
        model: qwen3:8b
      - upstream: cloud
        model: llama-3.3-70b
`

// env - getenv для тестов из map
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// TestParse_Valid - корректный конфиг разбирается, значения по умолчанию подставляются, ключ берется из окружения
func TestParse_Valid(t *testing.T) {
	cfg, err := Parse([]byte(validYAML), env(map[string]string{"CLOUD_KEY": "secret"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Server.Addr != "0.0.0.0:4000" || cfg.Server.ShutdownTimeout != DefaultShutdownTimeout {
		t.Fatalf("server = %+v", cfg.Server)
	}

	ollama := cfg.Upstreams["ollama"]
	if ollama.BaseURL != "http://localhost:11434/v1" || ollama.Timeout != DefaultTimeout || ollama.MaxConcurrent != DefaultMaxConcurrent {
		t.Fatalf("ollama = %+v, want defaults and base_url without trailing slash", ollama)
	}

	cloud := cfg.Upstreams["cloud"]
	if cloud.APIKey != "secret" || cloud.Timeout != 30*time.Second || cloud.MaxConcurrent != 8 {
		t.Fatalf("cloud = %+v", cloud)
	}

	if got := strings.Join(cfg.Aliases(), ","); got != "main,small" {
		t.Fatalf("Aliases = %s", got)
	}
	if got := cfg.Models["main"].Targets; len(got) != 2 || got[1] != (Target{Upstream: "cloud", Model: "llama-3.3-70b"}) {
		t.Fatalf("main targets = %+v", got)
	}
}

// TestParse_Defaults - без секции server используются адрес и таймаут по умолчанию
func TestParse_Defaults(t *testing.T) {
	cfg, err := Parse([]byte(`
upstreams:
  ollama: {base_url: "http://localhost:11434/v1"}
models:
  small: {targets: [{upstream: ollama, model: qwen3:4b}]}
`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != DefaultAddr || cfg.Server.ShutdownTimeout != DefaultShutdownTimeout {
		t.Fatalf("server = %+v", cfg.Server)
	}
}

// TestParse_Errors - ошибки конфига возвращаются с путем к полю, все сразу
func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "typo in field name",
			yaml: "upstreams:\n  ollama:\n    baseurl: http://x\n",
			want: []string{"field baseurl not found"},
		},
		{
			name: "not yaml",
			yaml: "upstreams: [",
			want: []string{"parse config"},
		},
		{
			name: "empty config",
			yaml: "server:\n  addr: 127.0.0.1:1\n",
			want: []string{"at least one upstream", "at least one model alias"},
		},
		{
			name: "bad upstream fields",
			yaml: `
upstreams:
  a: {base_url: "ftp://x", timeout: -1s, max_concurrent: -2}
  b: {base_url: "localhost:11434"}
models:
  m: {targets: [{upstream: a, model: x}]}
`,
			want: []string{
				`upstreams.a.base_url "ftp://x"`, "upstreams.a.timeout", "upstreams.a.max_concurrent",
				`upstreams.b.base_url "localhost:11434"`,
			},
		},
		{
			name: "bad model targets",
			yaml: `
upstreams:
  a: {base_url: "http://x"}
models:
  empty: {}
  broken: {targets: [{upstream: nope, model: x}, {upstream: a}]}
`,
			want: []string{
				"models.empty needs at least one target",
				`models.broken.targets[0]: unknown upstream "nope"`,
				"models.broken.targets[1]: model is required",
			},
		},
		{
			name: "missing api key",
			yaml: `
upstreams:
  cloud: {base_url: "https://api.example.com/v1", api_key_env: CLOUD_KEY}
models:
  m: {targets: [{upstream: cloud, model: x}]}
`,
			want: []string{"environment variable CLOUD_KEY is empty"},
		},
		{
			name: "bad duration",
			yaml: "server:\n  shutdown_timeout: soon\n",
			want: []string{"parse config"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml), env(nil))
			if err == nil {
				t.Fatal("expected error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestLoad - конфиг читается из файла, отсутствующий файл - ошибка
func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path, env(map[string]string{"CLOUD_KEY": "k"})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), env(nil)); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("err = %v, want read config error", err)
	}
}
