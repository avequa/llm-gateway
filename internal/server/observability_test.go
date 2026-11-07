package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// scrape - текст GET /metrics
func scrape(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d", rec.Code)
	}
	return rec.Body.String()
}

// mustContain - каждая строка want есть в метриках
func mustContain(t *testing.T, metrics string, want ...string) {
	t.Helper()
	for _, line := range want {
		if !strings.Contains(metrics, line) {
			t.Fatalf("metrics do not contain %q:\n%s", line, grepGateway(metrics))
		}
	}
}

// grepGateway - только метрики шлюза, без метрик Go и процесса, для читаемых сообщений об ошибке
func grepGateway(metrics string) string {
	var out []string
	for line := range strings.SplitSeq(metrics, "\n") {
		if strings.HasPrefix(line, "llm_gateway_") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// streamWithUsage - потоковый ответ, у которого последний кусок содержит usage, как при include_usage
func streamWithUsage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"po\"}}]}\n\n")
	w.(http.Flusher).Flush()
	_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ng\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
}

// TestMetrics_Request - успешный запрос учитывается в счетчике запросов, гистограмме длительности и токенах из usage
func TestMetrics_Request(t *testing.T) {
	up := newFakeUpstream(t, okResponse)
	s := gatewayFor(t, up.URL, "")

	if rec := post(t, s, `{"model":"main"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	mustContain(t, scrape(t, s),
		`llm_gateway_requests_total{model="main",status="200",target="local/qwen3:8b"} 1`,
		`llm_gateway_tokens_total{model="main",target="local/qwen3:8b",type="prompt"} 5`,
		`llm_gateway_tokens_total{model="main",target="local/qwen3:8b",type="completion"} 1`,
		`llm_gateway_request_duration_seconds_count{model="main",stream="false"} 1`,
		`llm_gateway_upstream_inflight_requests{upstream="local"} 0`,
		`llm_gateway_upstream_queue_waiting{upstream="local"} 0`,
		`llm_gateway_upstream_queue_wait_seconds_count{upstream="local"} 1`,
		`go_goroutines`,
	)
}

// TestMetrics_Stream - для потока шлюз просит usage у модели, считает токены из последнего куска и время до первого куска
func TestMetrics_Stream(t *testing.T) {
	up := newFakeUpstream(t, streamWithUsage)
	s := gatewayFor(t, up.URL, "")

	rec := post(t, s, `{"model":"main","stream":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"usage"`) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	options, ok := up.lastBody["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("stream_options = %v, want include_usage true", up.lastBody["stream_options"])
	}

	mustContain(t, scrape(t, s),
		`llm_gateway_tokens_total{model="main",target="local/qwen3:8b",type="prompt"} 7`,
		`llm_gateway_tokens_total{model="main",target="local/qwen3:8b",type="completion"} 2`,
		`llm_gateway_time_to_first_token_seconds_count{model="main",target="local/qwen3:8b"} 1`,
		`llm_gateway_request_duration_seconds_count{model="main",stream="true"} 1`,
	)
}

// TestStreamOptions_Kept - если агент сам задал stream_options, шлюз их не меняет
func TestStreamOptions_Kept(t *testing.T) {
	up := newFakeUpstream(t, streamWithUsage)
	s := gatewayFor(t, up.URL, "")

	post(t, s, `{"model":"main","stream":true,"stream_options":{"include_usage":false}}`)
	options, ok := up.lastBody["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != false {
		t.Fatalf("stream_options = %v, want the client value", up.lastBody["stream_options"])
	}

	post(t, s, `{"model":"main"}`)
	if _, set := up.lastBody["stream_options"]; set {
		t.Fatalf("stream_options added to a non-streaming request: %v", up.lastBody)
	}
}

// TestMetrics_Failures - неудачная попытка, неизвестная роль и неверный запрос попадают в метрики с понятными метками
func TestMetrics_Failures(t *testing.T) {
	first := statusUpstream(t, http.StatusNotFound)
	second := newFakeUpstream(t, okResponse)
	s := gatewayWith(t, first.URL, second.URL, `
  main: {targets: [{upstream: first, model: big}, {upstream: second, model: small}]}`)

	post(t, s, `{"model":"main"}`)
	post(t, s, `{"model":"gpt-4o"}`)
	post(t, s, `not json`)

	mustContain(t, scrape(t, s),
		`llm_gateway_target_failures_total{model="main",reason="404",target="first/big"} 1`,
		`llm_gateway_requests_total{model="main",status="200",target="second/small"} 1`,
		`llm_gateway_requests_total{model="unknown",status="404",target="none"} 1`,
		`llm_gateway_requests_total{model="unknown",status="400",target="none"} 1`,
	)
}

// TestTracing - на запрос пишутся span сервера, span попытки и span запроса к апстриму, traceparent уходит в апстрим
func TestTracing(t *testing.T) {
	var traceparent string
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		traceparent = r.Header.Get("Traceparent")
		okResponse(w, r)
	})

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	s := gatewayFor(t, up.URL, "", WithTracerProvider(provider))

	post(t, s, `{"model":"main"}`)
	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))

	spans := map[string]sdktrace.ReadOnlySpan{}
	for _, span := range recorder.Ended() {
		spans[span.Name()] = span
	}
	server, attempt, client := spans["POST /v1/chat/completions"], spans["attempt local/qwen3:8b"], spans["upstream local"]
	if len(spans) != 3 || server == nil || attempt == nil || client == nil {
		t.Fatalf("spans = %v, want server, attempt and upstream spans only", keys(spans))
	}

	traceID := server.SpanContext().TraceID()
	if attempt.Parent().SpanID() != server.SpanContext().SpanID() || client.Parent().SpanID() != attempt.SpanContext().SpanID() {
		t.Fatal("spans are not nested server -> attempt -> upstream")
	}
	if !strings.Contains(traceparent, traceID.String()) {
		t.Fatalf("traceparent = %q, want trace id %s", traceparent, traceID)
	}

	attrs := map[string]string{}
	for _, kv := range attempt.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["gen_ai.request.model"] != "qwen3:8b" || attrs["gen_ai.usage.output_tokens"] != "1" || attrs["llm_gateway.attempt"] != "1" {
		t.Fatalf("attempt attributes = %v", attrs)
	}
}

// keys - имена span для сообщения об ошибке
func keys(m map[string]sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSSEUsage - usage находится, даже если куски потока режут строку посередине, слишком длинная строка пропускается
func TestSSEUsage(t *testing.T) {
	stream := "data: {\"choices\":[]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n"
	for size := 1; size <= len(stream); size += 7 {
		s := &sseUsage{}
		for i := 0; i < len(stream); i += size {
			s.feed([]byte(stream[i:min(i+size, len(stream))]))
		}
		if s.found != (usage{PromptTokens: 11, CompletionTokens: 4}) {
			t.Fatalf("chunk size %d: found = %+v", size, s.found)
		}
	}

	s := &sseUsage{}
	s.feed([]byte("data: {\"usage\":{\"prompt_tokens\":1}," + strings.Repeat("x", maxUsageScanBytes) + "}\n"))
	s.feed([]byte("data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n"))
	if s.found != (usage{PromptTokens: 2, CompletionTokens: 3}) {
		t.Fatalf("after a too long line found = %+v", s.found)
	}
}

// TestCappedBuffer - usage берется из ответа, который поместился, и не берется из обрезанного
func TestCappedBuffer(t *testing.T) {
	small := &cappedBuffer{limit: 1000}
	_, _ = small.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":9}}`))
	if small.usage() != (usage{PromptTokens: 3, CompletionTokens: 9}) {
		t.Fatalf("usage = %+v", small.usage())
	}

	tiny := &cappedBuffer{limit: 10}
	if n, err := tiny.Write([]byte(`{"usage":{"prompt_tokens":3}}`)); n != 29 || err != nil {
		t.Fatalf("Write = %d, %v: must report the full length to keep io.TeeReader going", n, err)
	}
	if tiny.usage() != (usage{}) {
		t.Fatalf("usage from an overflowed buffer = %+v", tiny.usage())
	}
}
