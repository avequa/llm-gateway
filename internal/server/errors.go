package server

import (
	"encoding/json"
	"net/http"
)

// Типы ошибок в формате OpenAI API, клиенты (openai-python, LangChain) разбирают их по этому полю
const (
	errTypeInvalidRequest = "invalid_request_error"
	errTypeNotFound       = "not_found_error"
)

// apiError - тело ошибки в формате OpenAI: {"error": {"message", "type", "code"}}
type apiError struct {
	Error apiErrorBody `json:"error"`
}

// apiErrorBody - содержимое поля error
type apiErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    *string `json:"code"`
}

// writeError - отвечает ошибкой в формате OpenAI с заданным HTTP-кодом
func writeError(w http.ResponseWriter, status int, errType, code, message string) {
	body := apiError{Error: apiErrorBody{Message: message, Type: errType}}
	if code != "" {
		body.Error.Code = &code
	}
	writeJSON(w, status, body)
}

// writeJSON - отвечает JSON с заданным HTTP-кодом
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
