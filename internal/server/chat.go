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
	"strings"
	"time"
)

// maxRequestBytes - максимальный размер тела запроса: контекст агента с ответами tools бывает большим, но не бесконечным
const maxRequestBytes = 16 << 20

// errTypeAPI - тип ошибки OpenAI для сбоев на стороне шлюза и апстрима
const errTypeAPI = "api_error"

// headerTarget - заголовок ответа, в котором шлюз сообщает, какая модель на каком апстриме ответила
const headerTarget = "X-Gateway-Target"

// chatRequest - тело запроса, разобранное ровно настолько, чтобы заменить model и не потерять остальные поля
type chatRequest struct {
	fields map[string]json.RawMessage
	model  string
	stream bool
}

// handleChatCompletions - POST /v1/chat/completions: алиас заменяется на реальную модель, запрос уходит в апстрим
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	req, status, err := readChatRequest(w, r)
	if err != nil {
		writeError(w, status, errTypeInvalidRequest, "", err.Error())
		return
	}

	model, ok := s.cfg.Models[req.model]
	if !ok {
		writeError(w, http.StatusNotFound, errTypeInvalidRequest, "model_not_found",
			fmt.Sprintf("model %q does not exist, available models: %s", req.model, strings.Join(s.cfg.Aliases(), ", ")))
		return
	}

	target := model.Targets[0]
	body, err := req.withModel(target.Model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errTypeAPI, "", "cannot encode request: "+err.Error())
		return
	}

	resp, err := s.upstreams[target.Upstream].ChatCompletions(r.Context(), body)
	if err != nil {
		status, code := upstreamFailure(err)
		s.logger.Warn("upstream request failed", slog.String("model", req.model),
			slog.String("target", target.Upstream+"/"+target.Model), slog.String("error", err.Error()))
		writeError(w, status, errTypeAPI, code, fmt.Sprintf("upstream %s is not available: %v", target.Upstream, err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set(headerTarget, target.Upstream+"/"+target.Model)
	copyErr := copyResponse(w, resp)

	attrs := []slog.Attr{
		slog.String("model", req.model),
		slog.String("target", target.Upstream+"/"+target.Model),
		slog.Bool("stream", req.stream),
		slog.Int("status", resp.StatusCode),
		slog.Duration("duration", time.Since(start)),
	}
	level := slog.LevelInfo
	if resp.StatusCode >= http.StatusBadRequest {
		level = slog.LevelWarn
	}
	if copyErr != nil {
		level = slog.LevelWarn
		attrs = append(attrs, slog.String("error", "response was interrupted: "+copyErr.Error()))
	}
	s.logger.LogAttrs(r.Context(), level, "chat completion", attrs...)
}

// readChatRequest - читает тело с ограничением размера и достает model и stream; возвращает HTTP-код для ошибки
func readChatRequest(w http.ResponseWriter, r *http.Request) (chatRequest, int, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
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
	return req, 0, nil
}

// withModel - тело запроса с замененным model, все остальные поля (messages, tools, temperature) остаются как были
func (c chatRequest) withModel(model string) ([]byte, error) {
	fields := maps.Clone(c.fields)
	encoded, err := json.Marshal(model)
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

// copyResponse - передает клиенту код, тип и тело ответа апстрима; поток SSE отправляется кусками сразу, без буферизации
func copyResponse(w http.ResponseWriter, resp *http.Response) error {
	contentType := resp.Header.Get("Content-Type")
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	streaming := strings.HasPrefix(contentType, "text/event-stream")
	if streaming {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)

	if !streaming {
		_, err := io.Copy(w, resp.Body)
		return err
	}

	controller := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			if err := controller.Flush(); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
