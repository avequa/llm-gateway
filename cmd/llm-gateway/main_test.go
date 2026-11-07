package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig - пишет конфиг шлюза с заданным адресом во временный файл
func writeConfig(t *testing.T, addr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := fmt.Sprintf(`
server: {addr: %q, shutdown_timeout: 1s}
upstreams:
  ollama: {base_url: "http://localhost:11434/v1"}
models:
  main: {targets: [{upstream: ollama, model: qwen3:8b}, {upstream: ollama, model: qwen3:4b}]}
`, addr)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRun_ServesUntilCanceled - шлюз стартует, пишет маршруты, останавливается по отмене context с кодом 0
func TestRun_ServesUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	var stderr bytes.Buffer
	code := run(ctx, []string{"--config", writeConfig(t, "127.0.0.1:0"), "--log-format", "json"}, os.Getenv, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, logs:\n%s", code, stderr.String())
	}
	for _, want := range []string{`"targets":"ollama/qwen3:8b -> ollama/qwen3:4b"`, "gateway is listening", "gateway stopped"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("logs do not contain %q:\n%s", want, stderr.String())
		}
	}
}

// TestRun_Errors - неверные флаги и конфиг дают код 2, занятый порт - код 1
func TestRun_Errors(t *testing.T) {
	busy, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"unknown flag", []string{"--nope"}, 2},
		{"bad log format", []string{"--log-format", "xml"}, 2},
		{"bad log level", []string{"--log-level", "loud"}, 2},
		{"missing config", []string{"--config", filepath.Join(t.TempDir(), "nope.yaml")}, 2},
		{"port is busy", []string{"--config", writeConfig(t, busy.Addr().String())}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run(context.Background(), tt.args, os.Getenv, &bytes.Buffer{}); got != tt.want {
				t.Fatalf("code = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestHealthcheck - подкоманда healthcheck возвращает 0 для 200, 1 для другого кода и недоступного адреса, 2 для неверного флага
func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer ok.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer broken.Close()

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"healthy", []string{"healthcheck", "--url", ok.URL}, 0},
		{"bad status", []string{"healthcheck", "--url", broken.URL}, 1},
		{"unreachable", []string{"healthcheck", "--url", "http://127.0.0.1:1/healthz"}, 1},
		{"bad url", []string{"healthcheck", "--url", "://"}, 1},
		{"unknown flag", []string{"healthcheck", "--nope"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run(context.Background(), tt.args, os.Getenv, &bytes.Buffer{}); got != tt.want {
				t.Fatalf("code = %d, want %d", got, tt.want)
			}
		})
	}
}
