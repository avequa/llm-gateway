// Package upstream - HTTP-клиент к OpenAI-совместимому бэкенду: Ollama, vLLM, llama.cpp или облачный API
package upstream

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/avequa/llm-gateway/internal/config"
)

// userAgent - заголовок User-Agent запросов шлюза
const userAgent = "llm-gateway"

// Client - клиент одного апстрима
type Client struct {
	name    string
	baseURL string
	apiKey  string
	http    *http.Client
}

// New - создает клиента апстрима; timeout ограничивает ожидание начала ответа, а не всю генерацию
func New(name string, u config.Upstream) *Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: u.Timeout,
		MaxIdleConnsPerHost:   max(u.MaxConcurrent, 2),
		IdleConnTimeout:       90 * time.Second,
	}
	return &Client{name: name, baseURL: u.BaseURL, apiKey: u.APIKey, http: &http.Client{Transport: transport}}
}

// Name - имя апстрима из конфига
func (c *Client) Name() string {
	return c.name
}

// ChatCompletions - отправляет тело запроса в POST {base_url}/chat/completions и возвращает ответ как есть
// ответ нужно закрыть; отмена ctx (например, клиент шлюза отключился) прерывает запрос к апстриму
func (c *Client) ChatCompletions(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request to %s: %w", c.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: %w", c.name, err)
	}
	return resp, nil
}
