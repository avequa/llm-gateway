package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avequa/llm-gateway/internal/config"
)

// gatewayWith - шлюз с двумя апстримами first и second и произвольным описанием моделей
func gatewayWith(t *testing.T, firstURL, secondURL, models string) *Server {
	t.Helper()
	cfg, err := config.Parse(fmt.Appendf(nil, `
upstreams:
  first: {base_url: %q, timeout: 1s, max_concurrent: 1, queue_timeout: 100ms}
  second: {base_url: %q, timeout: 300ms}
models:
%s`, firstURL, secondURL, models), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, discardLogger())
}

// statusUpstream - апстрим, который всегда отвечает заданным кодом и телом ошибки
func statusUpstream(t *testing.T, status int) *fakeUpstream {
	t.Helper()
	return newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"status %d from upstream","type":"api_error"}}`, status)
	})
}

// TestFallback - при 404, 429, 5xx и недоступном апстриме запрос уходит следующей цели, при 400 - нет
func TestFallback(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()

	tests := []struct {
		name         string
		firstURL     func(t *testing.T) string
		wantStatus   int
		wantAttempts string
	}{
		{"model not found", func(t *testing.T) string { return statusUpstream(t, http.StatusNotFound).URL }, http.StatusOK, "2"},
		{"overloaded", func(t *testing.T) string { return statusUpstream(t, http.StatusTooManyRequests).URL }, http.StatusOK, "2"},
		{"server error", func(t *testing.T) string { return statusUpstream(t, http.StatusInternalServerError).URL }, http.StatusOK, "2"},
		{"connection refused", func(*testing.T) string { return downURL }, http.StatusOK, "2"},
		{"bad request is not retried", func(t *testing.T) string { return statusUpstream(t, http.StatusBadRequest).URL }, http.StatusBadRequest, "1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			second := newFakeUpstream(t, okResponse)
			s := gatewayWith(t, tt.firstURL(t), second.URL, `
  main: {targets: [{upstream: first, model: big}, {upstream: second, model: small}]}`)

			rec := post(t, s, `{"model":"main","messages":[]}`)
			if rec.Code != tt.wantStatus || rec.Header().Get(headerAttempts) != tt.wantAttempts {
				t.Fatalf("status = %d, attempts = %q, body = %s", rec.Code, rec.Header().Get(headerAttempts), rec.Body.String())
			}
			if tt.wantStatus == http.StatusOK && (rec.Header().Get(headerTarget) != "second/small" || second.lastBody["model"] != "small") {
				t.Fatalf("target = %q, second got %v", rec.Header().Get(headerTarget), second.lastBody)
			}
		})
	}
}

// TestFallback_AllFailed - если упали все цели, клиент получает ошибку последней цели как есть
func TestFallback_AllFailed(t *testing.T) {
	first := statusUpstream(t, http.StatusInternalServerError)
	second := statusUpstream(t, http.StatusNotFound)
	s := gatewayWith(t, first.URL, second.URL, `
  main: {targets: [{upstream: first, model: a}, {upstream: second, model: b}]}`)

	rec := post(t, s, `{"model":"main"}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "status 404 from upstream") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestQueue - при max_concurrent 1 второй запрос ждет первый, а после queue_timeout получает 429 с Retry-After
func TestQueue(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	first := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			<-release
		}
		okResponse(w, r)
	})
	s := gatewayWith(t, first.URL, first.URL, `
  main: {targets: [{upstream: first, model: a}, {upstream: first, model: b}]}`)

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- post(t, s, `{"model":"main"}`) }()
	time.Sleep(30 * time.Millisecond)

	start := time.Now()
	busy := post(t, s, `{"model":"main"}`)
	if busy.Code != http.StatusTooManyRequests || busy.Header().Get("Retry-After") == "" ||
		!strings.Contains(busy.Body.String(), `"code":"upstream_busy"`) {
		t.Fatalf("status = %d, headers = %v, body = %s", busy.Code, busy.Header(), busy.Body.String())
	}
	if waited := time.Since(start); waited > 250*time.Millisecond {
		t.Fatalf("waited %v: busy upstream must not be waited for twice", waited)
	}

	close(release)
	if first := <-done; first.Code != http.StatusOK {
		t.Fatalf("first request status = %d", first.Code)
	}
	if ok := post(t, s, `{"model":"main"}`); ok.Code != http.StatusOK {
		t.Fatalf("slot was not released: status = %d", ok.Code)
	}
}

// TestParams - params роли подставляются, если агент их не задал, max_tokens ограничивается потолком роли
func TestParams(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantMaxTokens float64
		wantEffort    string
		wantTemp      float64
	}{
		{"defaults applied", `{"model":"main"}`, 512, "none", 0.1},
		{"client values kept", `{"model":"main","reasoning_effort":"high","temperature":0.9,"max_tokens":100}`, 100, "high", 0.9},
		{"max_tokens capped", `{"model":"main","max_tokens":100000}`, 512, "none", 0.1},
		{"null max_tokens replaced", `{"model":"main","max_tokens":null}`, 512, "none", 0.1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := newFakeUpstream(t, okResponse)
			s := gatewayWith(t, up.URL, up.URL, `
  main:
    params: {reasoning_effort: none, temperature: 0.1}
    max_tokens: 512
    targets: [{upstream: first, model: a}]`)

			if rec := post(t, s, tt.body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got := up.lastBody
			if got["max_tokens"] != tt.wantMaxTokens || got["reasoning_effort"] != tt.wantEffort || got["temperature"] != tt.wantTemp {
				t.Fatalf("upstream body = %v", got)
			}
		})
	}
}

// TestParams_BadMaxTokens - max_tokens не числом или меньше 1 отклоняется до обращения к апстриму
func TestParams_BadMaxTokens(t *testing.T) {
	up := newFakeUpstream(t, func(http.ResponseWriter, *http.Request) { t.Error("upstream must not be called") })
	s := gatewayFor(t, up.URL, "")

	for _, body := range []string{`{"model":"main","max_tokens":"ten"}`, `{"model":"main","max_tokens":0}`} {
		if rec := post(t, s, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d", body, rec.Code)
		}
	}
}

// TestFallback_Streaming - fallback работает и для стриминга, пока клиенту еще ничего не отправлено
func TestFallback_Streaming(t *testing.T) {
	first := statusUpstream(t, http.StatusNotFound)
	second := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	})
	s := gatewayWith(t, first.URL, second.URL, `
  main: {targets: [{upstream: first, model: a}, {upstream: second, model: b}]}`)

	gw := httptest.NewServer(s)
	defer gw.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"main","stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)

	if resp.Header.Get(headerAttempts) != "2" || !strings.Contains(string(data), "[DONE]") {
		t.Fatalf("attempts = %q, body = %s", resp.Header.Get(headerAttempts), data)
	}
}
