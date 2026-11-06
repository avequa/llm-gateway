// Package server - HTTP-часть шлюза: маршруты OpenAI-совместимого API, служебные ручки и корректное завершение
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/avequa/llm-gateway/internal/config"
	"github.com/avequa/llm-gateway/internal/upstream"
)

// ownedBy - значение owned_by в списке моделей: алиасы принадлежат шлюзу, а не конкретному провайдеру
const ownedBy = "llm-gateway"

// Server - HTTP-обработчик шлюза
type Server struct {
	cfg       config.Config
	upstreams map[string]*upstream.Client
	logger    *slog.Logger
	started   time.Time
	mux       *http.ServeMux
}

// New - создает сервер и регистрирует маршруты
func New(cfg config.Config, logger *slog.Logger) *Server {
	s := &Server{
		cfg:       cfg,
		upstreams: make(map[string]*upstream.Client, len(cfg.Upstreams)),
		logger:    logger,
		started:   time.Now(),
		mux:       http.NewServeMux(),
	}
	for name, u := range cfg.Upstreams {
		s.upstreams[name] = upstream.New(name, u)
	}

	s.route(http.MethodGet, "/healthz", s.handleHealth)
	s.route(http.MethodGet, "/v1/models", s.handleModels)
	s.route(http.MethodPost, "/v1/chat/completions", s.handleChatCompletions)
	s.mux.HandleFunc("/", s.handleNotFound)
	return s
}

// route - регистрирует обработчик на метод и путь, остальные методы на этом пути получают 405 в формате OpenAI
func (s *Server) route(method, path string, handler http.HandlerFunc) {
	s.mux.HandleFunc(method+" "+path, handler)
	s.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", method)
		writeError(w, http.StatusMethodNotAllowed, errTypeInvalidRequest, "method_not_allowed",
			"method "+r.Method+" is not allowed for "+path+", use "+method)
	})
}

// ServeHTTP - передает запрос маршрутизатору, благодаря этому Server можно отдать http.Server как Handler
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleHealth - проба живости для Docker и Kubernetes
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// modelList - ответ GET /v1/models в формате OpenAI
type modelList struct {
	Object string  `json:"object"`
	Data   []model `json:"data"`
}

// model - одна модель в списке: для агента это алиас (small, main, validator), а не реальная модель
type model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// handleModels - список алиасов, которые обслуживает шлюз
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	list := modelList{Object: "list", Data: []model{}}
	for _, alias := range s.cfg.Aliases() {
		list.Data = append(list.Data, model{ID: alias, Object: "model", Created: s.started.Unix(), OwnedBy: ownedBy})
	}
	writeJSON(w, http.StatusOK, list)
}

// handleNotFound - неизвестный путь, ответ в формате OpenAI, чтобы клиент показал понятную ошибку
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, errTypeNotFound, "unknown_url", "unknown endpoint "+r.Method+" "+r.URL.Path)
}
