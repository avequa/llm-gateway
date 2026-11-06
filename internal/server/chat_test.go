package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/avequa/llm-gateway/internal/config"
	"github.com/avequa/llm-gateway/internal/upstream"
)

// fakeUpstream - тестовый OpenAI-совместимый апстрим, запоминает последний запрос
type fakeUpstream struct {
	*httptest.Server
	lastBody map[string]any
	lastAuth string
	lastPath string
}

// newFakeUpstream - поднимает апстрим с заданным обработчиком ответа
func newFakeUpstream(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastPath = r.URL.Path
		f.lastAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&f.lastBody)
		respond(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// okResponse - обычный ответ chat completions с моделью из запроса
func okResponse(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","model":"qwen3:8b","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
}

// gatewayFor - сервер шлюза с одним апстримом и алиасом main
func gatewayFor(t *testing.T, upstreamURL, extra string) *Server {
	t.Helper()
	cfg, err := config.Parse(fmt.Appendf(nil, `
upstreams:
  local: {base_url: %q, api_key_env: LOCAL_KEY, timeout: 300ms}
models:
  main: {targets: [{upstream: local, model: qwen3:8b}]}
%s`, upstreamURL, extra), func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, discardLogger())
}

// post - отправляет POST /v1/chat/completions с заданным телом
func post(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	s.ServeHTTP(rec, req)
	return rec
}

// TestChat_RewritesModel - алиас заменяется на реальную модель, остальные поля и ответ апстрима не меняются
func TestChat_RewritesModel(t *testing.T) {
	up := newFakeUpstream(t, okResponse)
	s := gatewayFor(t, up.URL, "")

	rec := post(t, s, `{"model":"main","messages":[{"role":"user","content":"ping"}],"temperature":0.2,"tools":[{"type":"function"}]}`)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"pong"`) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerTarget); got != "local/qwen3:8b" {
		t.Fatalf("%s = %q", headerTarget, got)
	}
	if up.lastPath != "/chat/completions" || up.lastAuth != "Bearer secret" {
		t.Fatalf("upstream path = %q, auth = %q", up.lastPath, up.lastAuth)
	}
	if up.lastBody["model"] != "qwen3:8b" || up.lastBody["temperature"] != 0.2 || up.lastBody["tools"] == nil || up.lastBody["messages"] == nil {
		t.Fatalf("upstream body = %v", up.lastBody)
	}
}

// TestChat_BadRequests - неверные запросы отклоняются до обращения к апстриму с ошибкой в формате OpenAI
func TestChat_BadRequests(t *testing.T) {
	up := newFakeUpstream(t, func(http.ResponseWriter, *http.Request) { t.Error("upstream must not be called") })
	s := gatewayFor(t, up.URL, "")

	tests := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{"not json", `hello`, http.StatusBadRequest, "must be a JSON object"},
		{"json array", `[]`, http.StatusBadRequest, "must be a JSON object"},
		{"no model", `{"messages":[]}`, http.StatusBadRequest, "field model is required"},
		{"model is a number", `{"model":1}`, http.StatusBadRequest, "field model is required"},
		{"stream is a string", `{"model":"main","stream":"yes"}`, http.StatusBadRequest, "field stream must be a boolean"},
		{"unknown alias", `{"model":"gpt-4o"}`, http.StatusNotFound, `model \"gpt-4o\" does not exist, available models: main`},
		{"too large", `{"model":"main","x":"` + strings.Repeat("a", maxRequestBytes) + `"}`, http.StatusRequestEntityTooLarge, "larger than"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(t, s, tt.body)
			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("status = %d, body = %s, want %d with %q", rec.Code, rec.Body.String(), tt.status, tt.want)
			}
			var body apiError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Message == "" {
				t.Fatalf("not an OpenAI error: %s", rec.Body.String())
			}
		})
	}
}

// TestChat_UpstreamError - ошибка апстрима (например, модель не скачана в Ollama) передается клиенту как есть
func TestChat_UpstreamError(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"model \"qwen3:8b\" not found, try pulling it first","type":"api_error"}}`)
	})

	rec := post(t, gatewayFor(t, up.URL, ""), `{"model":"main"}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "try pulling it first") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestChat_UpstreamUnavailable - выключенный апстрим дает 502, апстрим без ответа дольше timeout - 504
func TestChat_UpstreamUnavailable(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()

	release := make(chan struct{})
	defer close(release)
	slow := newFakeUpstream(t, func(http.ResponseWriter, *http.Request) { <-release })

	tests := []struct {
		name   string
		url    string
		status int
		code   string
	}{
		{"connection refused", downURL, http.StatusBadGateway, "upstream_unavailable"},
		{"no response headers in time", slow.URL, http.StatusGatewayTimeout, "upstream_timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(t, gatewayFor(t, tt.url, ""), `{"model":"main"}`)
			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), `"code":"`+tt.code+`"`) {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestChat_Streaming - куски SSE доходят до клиента сразу, пока апстрим еще генерирует ответ
func TestChat_Streaming(t *testing.T) {
	next := make(chan struct{})
	up := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"po\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-next
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ng\"}}]}\n\ndata: [DONE]\n\n")
	})

	gw := httptest.NewServer(gatewayFor(t, up.URL, ""))
	defer gw.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"main","stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get(headerTarget) != "local/qwen3:8b" {
		t.Fatalf("headers = %v", resp.Header)
	}

	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(first, `"po"`) {
		t.Fatalf("first chunk = %q, err = %v, want it before the upstream finished", first, err)
	}
	close(next)

	rest, _ := io.ReadAll(reader)
	if !strings.Contains(string(rest), `"ng"`) || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("rest = %q", rest)
	}
	if up.lastBody["stream"] != true {
		t.Fatalf("stream flag was not passed upstream: %v", up.lastBody)
	}
}

// TestChat_ClientDisconnect - если клиент отключился, запрос к апстриму отменяется и модель перестает генерировать
func TestChat_ClientDisconnect(t *testing.T) {
	canceled := make(chan struct{})
	up := newFakeUpstream(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(canceled)
	})
	s := gatewayFor(t, up.URL, "")
	s.upstreams["local"] = newClientWithoutTimeout(t, up.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"main"}`))
		s.ServeHTTP(httptest.NewRecorder(), req)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not canceled")
	}
	<-done
}

// newClientWithoutTimeout - клиент апстрима без таймаута, чтобы тест отмены не зависел от timeout
func newClientWithoutTimeout(t *testing.T, url string) *upstream.Client {
	t.Helper()
	return upstream.New("local", config.Upstream{BaseURL: url, MaxConcurrent: 1})
}
