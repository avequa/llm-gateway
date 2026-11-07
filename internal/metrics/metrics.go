// Package metrics - метрики Prometheus шлюза: запросы, латентность, токены, неудачные попытки и очередь
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// namespace - общий префикс имен метрик
const namespace = "llm_gateway"

// latencyBuckets - границы гистограмм времени в секундах: ответы LLM длятся от долей секунды до минут
var latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 80, 160}

// Metrics - все метрики шлюза в своем реестре
type Metrics struct {
	registry *prometheus.Registry

	requests       *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	firstToken     *prometheus.HistogramVec
	tokens         *prometheus.CounterVec
	targetFailures *prometheus.CounterVec
	inflight       *prometheus.GaugeVec
	queueWaiting   *prometheus.GaugeVec
	queueWait      *prometheus.HistogramVec
}

// New - создает метрики в новом реестре вместе со стандартными метриками Go и процесса
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "requests_total",
			Help: "Chat completion requests by model alias, target that answered and HTTP status returned to the client",
		}, []string{"model", "target", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "request_duration_seconds",
			Help: "Full chat completion duration from request start to the last byte sent", Buckets: latencyBuckets,
		}, []string{"model", "stream"}),
		firstToken: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "time_to_first_token_seconds",
			Help: "Streaming requests: time from request start to the first chunk sent to the client", Buckets: latencyBuckets,
		}, []string{"model", "target"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "tokens_total",
			Help: "Tokens reported by upstream usage, type is prompt or completion",
		}, []string{"model", "target", "type"}),
		targetFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "target_failures_total",
			Help: "Failed attempts that triggered fallback or ended the request, reason is an HTTP status, busy, timeout or unavailable",
		}, []string{"model", "target", "reason"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "upstream_inflight_requests",
			Help: "Requests currently holding an upstream slot",
		}, []string{"upstream"}),
		queueWaiting: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "upstream_queue_waiting",
			Help: "Requests currently waiting for an upstream slot",
		}, []string{"upstream"}),
		queueWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "upstream_queue_wait_seconds",
			Help: "Time spent waiting for an upstream slot", Buckets: latencyBuckets,
		}, []string{"upstream"}),
	}

	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.duration, m.firstToken, m.tokens, m.targetFailures, m.inflight, m.queueWaiting, m.queueWait,
	)
	return m
}

// Handler - HTTP-обработчик GET /metrics в формате Prometheus
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// Registry - реестр метрик, нужен тестам
func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

// Request - завершенный запрос: итоговый код, кто ответил и сколько длился
func (m *Metrics) Request(model, target string, status int, stream bool, duration time.Duration) {
	m.requests.WithLabelValues(model, target, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(model, strconv.FormatBool(stream)).Observe(duration.Seconds())
}

// FirstToken - время до первого куска потокового ответа
func (m *Metrics) FirstToken(model, target string, d time.Duration) {
	m.firstToken.WithLabelValues(model, target).Observe(d.Seconds())
}

// Tokens - токены из usage ответа модели
func (m *Metrics) Tokens(model, target string, prompt, completion int) {
	if prompt > 0 {
		m.tokens.WithLabelValues(model, target, "prompt").Add(float64(prompt))
	}
	if completion > 0 {
		m.tokens.WithLabelValues(model, target, "completion").Add(float64(completion))
	}
}

// TargetFailure - неудачная попытка цели с причиной
func (m *Metrics) TargetFailure(model, target, reason string) {
	m.targetFailures.WithLabelValues(model, target, reason).Inc()
}

// QueueEnter - запрос встал в очередь апстрима, возвращает функцию выхода из очереди с замером ожидания
func (m *Metrics) QueueEnter(upstream string) (leave func()) {
	start := time.Now()
	m.queueWaiting.WithLabelValues(upstream).Inc()
	return func() {
		m.queueWaiting.WithLabelValues(upstream).Dec()
		m.queueWait.WithLabelValues(upstream).Observe(time.Since(start).Seconds())
	}
}

// SlotTaken - запрос занял слот апстрима, возвращает функцию освобождения для метрики
func (m *Metrics) SlotTaken(upstream string) (released func()) {
	m.inflight.WithLabelValues(upstream).Inc()
	return func() { m.inflight.WithLabelValues(upstream).Dec() }
}
