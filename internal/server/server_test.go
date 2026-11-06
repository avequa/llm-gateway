package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/avequa/llm-gateway/internal/config"
)

// testConfig - конфиг с двумя алиасами для тестов
func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
upstreams:
  ollama: {base_url: "http://localhost:11434/v1"}
models:
  small: {targets: [{upstream: ollama, model: qwen3:4b}]}
  main: {targets: [{upstream: ollama, model: qwen3:8b}]}
`), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// discardLogger - логгер, который ничего не пишет
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// do - выполняет запрос к серверу и возвращает ответ
func do(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, path, nil))
	return rec
}

// TestHealth - /healthz отвечает 200 ok
func TestHealth(t *testing.T) {
	rec := do(t, New(testConfig(t), discardLogger()), http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

// TestModels - /v1/models возвращает алиасы по алфавиту в формате OpenAI
func TestModels(t *testing.T) {
	rec := do(t, New(testConfig(t), discardLogger()), http.MethodGet, "/v1/models")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d, content-type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	var list modelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 2 {
		t.Fatalf("list = %+v", list)
	}
	for i, want := range []string{"main", "small"} {
		m := list.Data[i]
		if m.ID != want || m.Object != "model" || m.OwnedBy != ownedBy || m.Created == 0 {
			t.Fatalf("data[%d] = %+v, want id %q", i, m, want)
		}
	}
}

// TestNotFound - неизвестный путь отвечает 404 с ошибкой в формате OpenAI
func TestNotFound(t *testing.T) {
	rec := do(t, New(testConfig(t), discardLogger()), http.MethodPost, "/v1/embeddings")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}

	var body apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != errTypeNotFound || body.Error.Code == nil || *body.Error.Code != "unknown_url" ||
		!strings.Contains(body.Error.Message, "POST /v1/embeddings") {
		t.Fatalf("error = %+v", body.Error)
	}
}

// TestWrongMethod - неверный метод на известном пути отвечает 405 с заголовком Allow и ошибкой в формате OpenAI
func TestWrongMethod(t *testing.T) {
	rec := do(t, New(testConfig(t), discardLogger()), http.MethodPost, "/v1/models")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("status = %d, Allow = %q, want 405 and GET", rec.Code, rec.Header().Get("Allow"))
	}
	if !strings.Contains(rec.Body.String(), `"code":"method_not_allowed"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestWriteError_NoCode - без кода поле code в JSON равно null, как в OpenAI
func TestWriteError_NoCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusBadRequest, errTypeInvalidRequest, "", "bad request")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":null`) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
