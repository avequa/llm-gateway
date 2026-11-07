package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/avequa/llm-gateway/internal/config"
	"github.com/avequa/llm-gateway/internal/upstream"
)

// maxRequestBytes - максимальный размер тела запроса: контекст агента с ответами tools бывает большим, но не бесконечным
const maxRequestBytes = 16 << 20

// maxErrorBodyBytes - сколько байт ошибки апстрима запоминать, чтобы вернуть ее клиенту, если все цели упали
const maxErrorBodyBytes = 64 << 10

// statusClientClosed - код для метрик и логов, когда клиент отключился раньше ответа (как 499 в NGINX)
const statusClientClosed = 499

// Метки для запросов, которые не дошли до выбора цели
const (
	labelUnknownModel = "unknown"
	labelNoTarget     = "none"
)

// Типы ошибок OpenAI для сбоев на стороне шлюза и апстрима
const (
	errTypeAPI       = "api_error"
	errTypeRateLimit = "rate_limit_error"
)

// Заголовки ответа: какая модель ответила и с какой попытки
const (
	headerTarget   = "X-Gateway-Target"
	headerAttempts = "X-Gateway-Attempts"
)

// chatRequest - тело запроса, разобранное ровно настолько, чтобы заменить model и не потерять остальные поля
type chatRequest struct {
	fields map[string]json.RawMessage
	model  string
	stream bool
}

// failure - неудачная попытка: либо ответ апстрима с ошибкой (body), либо ошибка шлюза (code и message)
type failure struct {
	busy        bool
	status      int
	contentType string
	body        []byte
	errType     string
	code        string
	message     string
}

// handleChatCompletions - POST /v1/chat/completions: алиас заменяется на реальную модель, цели пробуются по порядку
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	span := trace.SpanFromContext(r.Context())
	span.SetAttributes(attribute.String("gen_ai.operation.name", "chat"))

	req, status, err := readChatRequest(w, r)
	if err != nil {
		s.metrics.Request(labelUnknownModel, labelNoTarget, status, false, time.Since(start))
		writeError(w, status, errTypeInvalidRequest, "", err.Error())
		return
	}
	span.SetAttributes(attribute.String("llm_gateway.model", req.model), attribute.Bool("llm_gateway.stream", req.stream))

	model, ok := s.cfg.Models[req.model]
	if !ok {
		s.metrics.Request(labelUnknownModel, labelNoTarget, http.StatusNotFound, req.stream, time.Since(start))
		writeError(w, http.StatusNotFound, errTypeInvalidRequest, "model_not_found",
			fmt.Sprintf("model %q does not exist, available models: %s", req.model, strings.Join(s.cfg.Aliases(), ", ")))
		return
	}

	var last failure
	lastTarget := labelNoTarget
	busy := make(map[string]bool)
	for i, target := range model.Targets {
		if busy[target.Upstream] {
			continue
		}
		attempt := i + 1
		lastTarget = targetName(target)

		last, ok = s.tryTarget(w, r, req, model, target, attempt, start)
		if ok {
			return
		}
		if r.Context().Err() != nil {
			s.metrics.Request(req.model, lastTarget, statusClientClosed, req.stream, time.Since(start))
			return
		}
		if last.busy {
			busy[target.Upstream] = true
		}
		s.metrics.TargetFailure(req.model, lastTarget, last.metricReason())
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "target failed", s.withTrace(r.Context(),
			slog.String("model", req.model),
			slog.String("target", lastTarget),
			slog.Int("attempt", attempt),
			slog.Int("status", last.status),
			slog.String("reason", last.reason()),
		)...)
	}

	s.metrics.Request(req.model, lastTarget, last.status, req.stream, time.Since(start))
	span.SetStatus(codes.Error, "all targets failed")
	s.logger.LogAttrs(r.Context(), slog.LevelWarn, "all targets failed", s.withTrace(r.Context(),
		slog.String("model", req.model), slog.Duration("duration", time.Since(start)))...)
	last.write(w)
}

// tryTarget - одна попытка: занять слот апстрима, отправить запрос и, если ответ не требует fallback, передать его клиенту
func (s *Server) tryTarget(w http.ResponseWriter, r *http.Request, req chatRequest, model config.Model, target config.Target,
	attempt int, start time.Time,
) (f failure, ok bool) {
	name := targetName(target)
	ctx, span := s.tracer.Start(r.Context(), "attempt "+name, trace.WithAttributes(
		attribute.Int("llm_gateway.attempt", attempt),
		attribute.String("llm_gateway.upstream", target.Upstream),
		attribute.String("gen_ai.request.model", target.Model),
	))
	defer func() {
		if !ok {
			span.SetStatus(codes.Error, f.metricReason())
		}
		span.End()
	}()

	client := s.upstreams[target.Upstream]

	leaveQueue := s.metrics.QueueEnter(target.Upstream)
	release, err := client.Acquire(ctx)
	leaveQueue()
	if errors.Is(err, upstream.ErrBusy) {
		return failure{
			busy: true, status: http.StatusTooManyRequests, errType: errTypeRateLimit, code: "upstream_busy",
			message: fmt.Sprintf("all upstreams for model %q are busy, retry later: %v", req.model, err),
		}, false
	}
	if err != nil {
		return failure{status: statusClientClosed, errType: errTypeAPI, message: err.Error()}, false
	}
	defer release()
	defer s.metrics.SlotTaken(target.Upstream)()

	body, err := req.bodyFor(model, target.Model)
	if err != nil {
		return failure{status: http.StatusInternalServerError, errType: errTypeAPI, message: "cannot encode request: " + err.Error()}, false
	}

	resp, err := client.ChatCompletions(ctx, body)
	if err != nil {
		status, code := upstreamFailure(err)
		return failure{
			status: status, errType: errTypeAPI, code: code,
			message: fmt.Sprintf("upstream %s is not available: %v", target.Upstream, err),
		}, false
	}
	defer func() { _ = resp.Body.Close() }()
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))

	if shouldFallback(resp.StatusCode) {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return failure{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: data}, false
	}

	w.Header().Set(headerTarget, name)
	w.Header().Set(headerAttempts, strconv.Itoa(attempt))
	result := copyResponse(w, resp, func() {
		if req.stream {
			s.metrics.FirstToken(req.model, name, time.Since(start))
		}
	})

	s.metrics.Request(req.model, name, resp.StatusCode, req.stream, time.Since(start))
	s.metrics.Tokens(req.model, name, result.usage.PromptTokens, result.usage.CompletionTokens)
	span.SetAttributes(
		attribute.Int("gen_ai.usage.input_tokens", result.usage.PromptTokens),
		attribute.Int("gen_ai.usage.output_tokens", result.usage.CompletionTokens),
	)
	s.logCompletion(r.Context(), req, name, attempt, resp.StatusCode, start, result)
	return failure{}, true
}

// logCompletion - одна строка лога на завершенный запрос
func (s *Server) logCompletion(ctx context.Context, req chatRequest, target string, attempt, status int, start time.Time, result copyResult) {
	attrs := []slog.Attr{
		slog.String("model", req.model),
		slog.String("target", target),
		slog.Int("attempt", attempt),
		slog.Bool("stream", req.stream),
		slog.Int("status", status),
		slog.Duration("duration", time.Since(start)),
	}
	if result.usage.PromptTokens > 0 || result.usage.CompletionTokens > 0 {
		attrs = append(attrs, slog.Int("promptTokens", result.usage.PromptTokens), slog.Int("completionTokens", result.usage.CompletionTokens))
	}
	level := slog.LevelInfo
	if status >= http.StatusBadRequest {
		level = slog.LevelWarn
	}
	if result.err != nil {
		level = slog.LevelWarn
		attrs = append(attrs, slog.String("error", "response was interrupted: "+result.err.Error()))
	}
	s.logger.LogAttrs(ctx, level, "chat completion", s.withTrace(ctx, attrs...)...)
}

// withTrace - добавляет к атрибутам лога traceId, чтобы по строке лога найти трейс в Jaeger
func (s *Server) withTrace(ctx context.Context, attrs ...slog.Attr) []slog.Attr {
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		attrs = append(attrs, slog.String("traceId", sc.TraceID().String()))
	}
	return attrs
}

// targetName - имя цели для заголовков, логов и метрик: upstream/model
func targetName(t config.Target) string {
	return t.Upstream + "/" + t.Model
}

// shouldFallback - стоит ли пробовать следующую цель: модель не найдена, перегрузка, таймаут или ошибка сервера
// при 400 и других ошибках запроса следующая цель ответит так же, поэтому ответ сразу уходит клиенту
func shouldFallback(status int) bool {
	return status == http.StatusNotFound || status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// reason - короткое описание причины неудачи для лога
func (f failure) reason() string {
	if f.message != "" {
		return f.message
	}
	return strings.TrimSpace(string(f.body))
}

// metricReason - причина неудачи для метки метрики: busy, timeout, unavailable или HTTP-код апстрима
func (f failure) metricReason() string {
	switch {
	case f.busy:
		return "busy"
	case f.code == "upstream_timeout":
		return "timeout"
	case f.code == "upstream_unavailable":
		return "unavailable"
	default:
		return strconv.Itoa(f.status)
	}
}

// write - отвечает клиенту последней неудачей: ошибку апстрима передает как есть, ошибку шлюза - в формате OpenAI
func (f failure) write(w http.ResponseWriter) {
	if f.body != nil {
		if f.contentType != "" {
			w.Header().Set("Content-Type", f.contentType)
		}
		w.WriteHeader(f.status)
		_, _ = w.Write(f.body)
		return
	}
	if f.status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "5")
	}
	writeError(w, f.status, f.errType, f.code, f.message)
}

// readChatRequest - читает тело с ограничением размера и достает model и stream; возвращает HTTP-код для ошибки
func readChatRequest(w http.ResponseWriter, r *http.Request) (chatRequest, int, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			return chatRequest{}, http.StatusRequestEntityTooLarge, fmt.Errorf("request body is larger than %d bytes", maxRequestBytes)
		}
		return chatRequest{}, http.StatusBadRequest, fmt.Errorf("cannot read request body: %w", err)
	}

	req := chatRequest{}
	if err := json.Unmarshal(data, &req.fields); err != nil || req.fields == nil {
		return chatRequest{}, http.StatusBadRequest, errors.New("request body must be a JSON object")
	}
	if err := json.Unmarshal(req.fields["model"], &req.model); err != nil || req.model == "" {
		return chatRequest{}, http.StatusBadRequest, errors.New("field model is required and must be a string")
	}
	if raw, ok := req.fields["stream"]; ok {
		if err := json.Unmarshal(raw, &req.stream); err != nil {
			return chatRequest{}, http.StatusBadRequest, errors.New("field stream must be a boolean")
		}
	}
	if raw, ok := req.fields["max_tokens"]; ok && string(raw) != "null" {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil || n < 1 {
			return chatRequest{}, http.StatusBadRequest, errors.New("field max_tokens must be a positive integer")
		}
	}
	return req, 0, nil
}

// bodyFor - тело запроса для конкретной цели: реальная модель, params роли там, где агент их не задал, потолок max_tokens
// и stream_options.include_usage для потока, чтобы модель прислала счетчики токенов в последнем куске
func (c chatRequest) bodyFor(model config.Model, realModel string) ([]byte, error) {
	fields := maps.Clone(c.fields)

	for key, value := range model.Params {
		if _, set := fields[key]; set {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("param %s: %w", key, err)
		}
		fields[key] = encoded
	}

	if model.MaxTokens > 0 {
		var requested int
		raw, set := fields["max_tokens"]
		if !set || string(raw) == "null" || json.Unmarshal(raw, &requested) != nil || requested > model.MaxTokens {
			fields["max_tokens"] = json.RawMessage(strconv.Itoa(model.MaxTokens))
		}
	}

	if _, set := fields["stream_options"]; c.stream && !set {
		fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}

	encoded, err := json.Marshal(realModel)
	if err != nil {
		return nil, err
	}
	fields["model"] = encoded
	return json.Marshal(fields)
}

// upstreamFailure - HTTP-код и код ошибки для клиента, если апстрим не ответил
func upstreamFailure(err error) (status int, code string) {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return http.StatusGatewayTimeout, "upstream_timeout"
	}
	return http.StatusBadGateway, "upstream_unavailable"
}
