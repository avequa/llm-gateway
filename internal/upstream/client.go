// Package upstream - HTTP-клиент к OpenAI-совместимому бэкенду: Ollama, vLLM, llama.cpp или облачный API
package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/avequa/llm-gateway/internal/config"
)

// userAgent - заголовок User-Agent запросов шлюза
const userAgent = "llm-gateway"

// ErrBusy - все слоты апстрима заняты дольше queue_timeout
var ErrBusy = errors.New("upstream is busy")

// Client - клиент одного апстрима с ограничением одновременных запросов
type Client struct {
	name         string
	baseURL      string
	apiKey       string
	http         *http.Client
	slots        chan struct{}
	queueTimeout time.Duration
}

// Option - необязательная настройка клиента
type Option func(*options)

// options - собранные необязательные настройки
type options struct {
	tracerProvider trace.TracerProvider
}

// WithTracerProvider - писать span на каждый HTTP-запрос к апстриму и передавать заголовок traceparent
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProvider = tp }
}

// New - создает клиента апстрима; timeout ограничивает ожидание начала ответа, а не всю генерацию
func New(name string, u config.Upstream, opts ...Option) *Client {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: u.Timeout,
		MaxIdleConnsPerHost:   max(u.MaxConcurrent, 2),
		IdleConnTimeout:       90 * time.Second,
	}
	var roundTripper http.RoundTripper = transport
	if o.tracerProvider != nil {
		roundTripper = otelhttp.NewTransport(transport,
			otelhttp.WithTracerProvider(o.tracerProvider),
			otelhttp.WithPropagators(propagation.TraceContext{}),
			otelhttp.WithSpanNameFormatter(func(string, *http.Request) string { return "upstream " + name }),
		)
	}

	return &Client{
		name:         name,
		baseURL:      u.BaseURL,
		apiKey:       u.APIKey,
		http:         &http.Client{Transport: roundTripper},
		slots:        make(chan struct{}, max(u.MaxConcurrent, 1)),
		queueTimeout: u.QueueTimeout,
	}
}

// Acquire - занимает слот апстрима; если все заняты, ждет до queue_timeout и возвращает ErrBusy
// слот нужно вернуть вызовом release после того, как ответ полностью прочитан
func (c *Client) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case c.slots <- struct{}{}:
		return c.releaseOnce(), nil
	default:
	}

	wait := context.Background()
	if c.queueTimeout > 0 {
		var cancel context.CancelFunc
		wait, cancel = context.WithTimeout(wait, c.queueTimeout)
		defer cancel()
	}

	select {
	case c.slots <- struct{}{}:
		return c.releaseOnce(), nil
	case <-wait.Done():
		return nil, fmt.Errorf("%w: %s has %d of %d requests in progress for %s", ErrBusy, c.name, len(c.slots), cap(c.slots), c.queueTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// releaseOnce - функция возврата слота, повторный вызов ничего не делает
func (c *Client) releaseOnce() func() {
	var once sync.Once
	return func() { once.Do(func() { <-c.slots }) }
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
