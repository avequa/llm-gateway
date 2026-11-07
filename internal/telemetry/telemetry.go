// Package telemetry - трейсы OpenTelemetry: экспорт по OTLP, если он настроен переменными окружения, иначе выключены
package telemetry

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// defaultServiceName - имя сервиса в трейсах, если не задан OTEL_SERVICE_NAME
const defaultServiceName = "llm-gateway"

// Setup - создает провайдер трейсов: при заданном OTEL_EXPORTER_OTLP_ENDPOINT (или ..._TRACES_ENDPOINT) трейсы
// уходят по OTLP/HTTP, иначе возвращается провайдер, который ничего не пишет; shutdown дописывает буфер при остановке
func Setup(ctx context.Context, version string, getenv func(string) string) (trace.TracerProvider, func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	if !Enabled(getenv) {
		return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	serviceName := getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = defaultServiceName
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", serviceName),
		attribute.String("service.version", version),
	))
	if err != nil && !errors.Is(err, resource.ErrSchemaURLConflict) {
		return nil, nil, fmt.Errorf("create resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	return provider, provider.Shutdown, nil
}

// Enabled - задан ли адрес OTLP, то есть будут ли трейсы куда-то отправляться
func Enabled(getenv func(string) string) bool {
	return getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}
