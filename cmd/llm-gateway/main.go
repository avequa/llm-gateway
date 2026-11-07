// Package main - LLM-gateway: OpenAI-совместимый прокси перед Ollama и облачными API
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/avequa/llm-gateway/internal/config"
	"github.com/avequa/llm-gateway/internal/server"
	"github.com/avequa/llm-gateway/internal/telemetry"
)

// version - версия шлюза, задается при сборке: -ldflags "-X main.version=1.2.3"
var version = "dev"

// telemetryFlushTimeout - сколько ждать отправки накопленных трейсов при остановке
const telemetryFlushTimeout = 5 * time.Second

// main - запускает шлюз, Ctrl+C и SIGTERM завершают его корректно
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stderr)
	stop()
	os.Exit(code)
}

// run - разбирает флаги, загружает конфиг и обслуживает HTTP до отмены context; логи пишутся в stderr
// первый аргумент healthcheck запускает проверку здоровья вместо сервера
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "healthcheck" {
		return runHealthcheck(ctx, args[1:], stderr)
	}

	fs := flag.NewFlagSet("llm-gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to the YAML config")
	logFormat := fs.String("log-format", "text", "text or json")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger, err := newLogger(stderr, *logFormat, *logLevel)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	cfg, err := config.Load(*configPath, getenv)
	if err != nil {
		logger.Error("cannot load config", slog.String("error", err.Error()))
		return 2
	}
	logRoutes(logger, cfg)

	tracerProvider, flushTraces, err := telemetry.Setup(ctx, version, getenv)
	if err != nil {
		logger.Error("cannot set up tracing", slog.String("error", err.Error()))
		return 1
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryFlushTimeout)
		defer cancel()
		if flushErr := flushTraces(flushCtx); flushErr != nil {
			logger.Warn("cannot flush traces", slog.String("error", flushErr.Error()))
		}
	}()
	logger.Info("telemetry", slog.String("version", version), slog.Bool("tracing", telemetry.Enabled(getenv)),
		slog.String("metrics", "/metrics"))

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Server.Addr)
	if err != nil {
		logger.Error("cannot listen", slog.String("addr", cfg.Server.Addr), slog.String("error", err.Error()))
		return 1
	}

	handler := server.New(cfg, logger, server.WithTracerProvider(tracerProvider))
	if err := server.Run(ctx, listener, handler, cfg.Server.ShutdownTimeout, logger); err != nil {
		logger.Error("server stopped with error", slog.String("error", err.Error()))
		return 1
	}
	return 0
}

// newLogger - slog-логгер в формате text (для человека) или json (для сбора логов)
func newLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}

	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("invalid --log-format %q (use text or json)", format)
	}
}

// logRoutes - пишет в лог, какой алиас в какую цепочку моделей ведет
func logRoutes(logger *slog.Logger, cfg config.Config) {
	for _, alias := range cfg.Aliases() {
		var chain []string
		for _, t := range cfg.Models[alias].Targets {
			chain = append(chain, t.Upstream+"/"+t.Model)
		}
		model := cfg.Models[alias]
		logger.Info("route", slog.String("model", alias), slog.String("targets", strings.Join(chain, " -> ")),
			slog.Any("params", model.Params), slog.Int("maxTokens", model.MaxTokens))
	}
}
