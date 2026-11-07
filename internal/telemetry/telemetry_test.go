package telemetry

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// env - getenv для тестов из map
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// TestSetup_Disabled - без адреса OTLP трейсы выключены: провайдер ничего не пишет, остановка без ошибок
func TestSetup_Disabled(t *testing.T) {
	provider, shutdown, err := Setup(context.Background(), "test", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(noop.TracerProvider); !ok {
		t.Fatalf("provider = %T, want noop", provider)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestSetup_Enabled - с адресом OTLP создается настоящий провайдер, остановка без накопленных span проходит быстро
func TestSetup_Enabled(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	provider, shutdown, err := Setup(context.Background(), "test", env(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*sdktrace.TracerProvider); !ok {
		t.Fatalf("provider = %T, want SDK provider", provider)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if !Enabled(env(map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "x"})) || Enabled(env(nil)) {
		t.Fatal("Enabled reads the wrong variables")
	}
}
