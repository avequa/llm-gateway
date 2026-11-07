# llm-gateway

HTTP-шлюз к LLM с API как у OpenAI. Клиент пишет в поле `model` не модель, а роль из конфига (`small`, `main`, `validator`). Шлюз отправляет запрос первой модели роли, а если она не ответила, пробует следующую. К каждому апстриму держит очередь, пишет метрики Prometheus, трейсы OpenTelemetry и логи.

Используется в проекте Incident Copilot: агенты на LangGraph ходят к моделям только через этот шлюз.

## Запуск в Docker

Нужен Docker с поддержкой NVIDIA GPU (Docker Desktop с WSL 2 или Docker Engine с NVIDIA Container Toolkit).

```bash
cp .env.example .env          # путь к уже скачанным моделям Ollama и пароль Grafana
make up                       # шлюз, Ollama, Prometheus, Alertmanager, Grafana, Jaeger
make pull MODEL=qwen3:4b      # скачать модель, если её ещё нет
make smoke                    # 5 коротких запросов к роли small
```

| Сервис | Адрес |
|---|---|
| шлюз | http://127.0.0.1:4000 |
| Ollama | http://127.0.0.1:11434 |
| Grafana (admin / пароль из `.env`) | http://127.0.0.1:3000 |
| Prometheus | http://127.0.0.1:9090 |
| Alertmanager | http://127.0.0.1:9093 |
| Jaeger | http://127.0.0.1:16686 |

Остановить: `make down`. Посмотреть модели и что сейчас в видеопамяти: `make models`.

Если Ollama уже стоит на машине как служба, её нужно выключить, иначе две Ollama будут делить видеокарту и порт 11434:

```bash
sudo systemctl disable --now ollama
```

Конфиг шлюза для Docker лежит в `deploy/gateway.yaml`.

## Запуск без Docker

Нужны Go 1.26+ и Ollama на `localhost:11434`.

```bash
ollama pull qwen3:4b
cp config.example.yaml config.yaml
make run
```

## Запрос

```bash
curl -i localhost:4000/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"small","messages":[{"role":"user","content":"Скажи одно слово: привет"}]}'
```

В ответе есть заголовки `X-Gateway-Target` (какая модель ответила) и `X-Gateway-Attempts` (с какой попытки).

Из Python:

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:4000/v1", api_key="unused")
resp = client.chat.completions.create(model="main", messages=[{"role": "user", "content": "привет"}])
```

```python
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(base_url="http://127.0.0.1:4000/v1", api_key="unused", model="main")
```

Тело запроса передаётся в модель как есть, меняется только `model`. Поэтому `tools`, `response_format`, `stream` и остальные поля работают так же, как с OpenAI.

## Конфигурация

```yaml
server:
  addr: 127.0.0.1:4000
  shutdown_timeout: 15s

upstreams:
  ollama:
    base_url: http://localhost:11434/v1
    timeout: 120s
    max_concurrent: 1
    queue_timeout: 180s

models:
  small:
    params:
      reasoning_effort: none
      temperature: 0.1
    max_tokens: 512
    targets:
      - {upstream: ollama, model: qwen3:1.7b}
      - {upstream: ollama, model: qwen3:4b}
  validator:
    params:
      temperature: 0
    max_tokens: 1024
    targets:
      - {upstream: ollama, model: gemma3:4b}
```

| Поле | По умолчанию | Что значит |
|---|---|---|
| `server.addr` | `127.0.0.1:4000` | адрес шлюза |
| `server.shutdown_timeout` | `15s` | сколько ждать активные запросы при остановке |
| `upstreams.<имя>.base_url` | обязательно | адрес OpenAI-совместимого API, заканчивается на `/v1` |
| `upstreams.<имя>.api_key_env` | нет | имя переменной окружения с ключом API |
| `upstreams.<имя>.timeout` | `120s` | сколько ждать начала ответа модели |
| `upstreams.<имя>.max_concurrent` | `4` | сколько запросов одновременно отправлять в апстрим |
| `upstreams.<имя>.queue_timeout` | `60s` | сколько запрос может ждать в очереди |
| `models.<роль>.targets` | обязательно | модели по порядку: `{upstream, model}` |
| `models.<роль>.params` | нет | значения полей запроса, если клиент их не передал. Нельзя задавать `model`, `messages`, `stream` |
| `models.<роль>.max_tokens` | нет | максимум для `max_tokens`, большее значение урезается |

Неизвестные поля в конфиге считаются ошибкой. При запуске выводятся сразу все ошибки конфига.

Флаги: `--config` (по умолчанию `config.yaml`), `--log-format text|json`, `--log-level debug|info|warn|error`.

Переменные окружения: `OTEL_EXPORTER_OTLP_ENDPOINT` включает отправку трейсов (например, `http://jaeger:4318`), `OTEL_SERVICE_NAME` меняет имя сервиса в трейсах (по умолчанию `llm-gateway`).

Подкоманда `llm-gateway healthcheck` проверяет `/healthz` и завершается с кодом 0, если шлюз отвечает, 1 если нет, 2 при неверных аргументах. Она нужна для HEALTHCHECK в образе без shell.

## API

| Метод и путь | Что возвращает |
|---|---|
| `POST /v1/chat/completions` | ответ модели в формате OpenAI, JSON или поток SSE |
| `GET /v1/models` | список ролей |
| `GET /metrics` | метрики Prometheus |
| `GET /healthz` | `ok` |

Когда шлюз переходит к следующей модели роли: ответ 404, 408, 429, 5xx, таймаут или сетевая ошибка. На остальные ошибки (например, 400) ответ модели сразу уходит клиенту.

| Ситуация | Ответ |
|---|---|
| неизвестная роль | 404 `model_not_found`, в тексте список ролей |
| неправильное тело или `max_tokens` | 400 |
| тело больше 16 MiB | 413 |
| запрос ждал в очереди дольше `queue_timeout` | 429 `upstream_busy`, заголовок `Retry-After: 5` |
| ни одна модель роли не ответила | 502 `upstream_unavailable` или 504 `upstream_timeout` |

Ошибки возвращаются в формате OpenAI: `{"error": {"message", "type", "code"}}`.

Если клиент отключился посреди ответа, запрос к модели отменяется.

## Метрики, алерты, трейсы

| Метрика | Тип | Метки |
|---|---|---|
| `llm_gateway_requests_total` | counter | `model`, `target`, `status` |
| `llm_gateway_request_duration_seconds` | histogram | `model`, `stream` |
| `llm_gateway_time_to_first_token_seconds` | histogram | `model`, `target` |
| `llm_gateway_tokens_total` | counter | `model`, `target`, `type` |
| `llm_gateway_target_failures_total` | counter | `model`, `target`, `reason` |
| `llm_gateway_upstream_inflight_requests` | gauge | `upstream` |
| `llm_gateway_upstream_queue_waiting` | gauge | `upstream` |
| `llm_gateway_upstream_queue_wait_seconds` | histogram | `upstream` |

`model` это роль, `target` это модель, которая ответила. Токены берутся из `usage` ответа. Для потоковых запросов шлюз сам добавляет `stream_options.include_usage`.

Алерты в `deploy/prometheus/alerts.yml`:

| Алерт | Условие |
|---|---|
| LLMGatewayDown | шлюз недоступен 1 минуту |
| LLMHighErrorRate | больше 20% ответов 5xx и 429 за 5 минут |
| LLMSlowResponses | p95 ответа больше 60 с в течение 10 минут |
| LLMSlowFirstToken | p95 до первого токена больше 15 с в течение 10 минут |
| LLMFrequentFallback | больше 5 неудачных попыток одной модели за 15 минут |
| LLMQueueBacklog | в очереди больше 2 запросов в течение 5 минут |

Alertmanager по умолчанию только показывает алерты. Отправка в Telegram закомментирована в `deploy/alertmanager/alertmanager.yml`.

Дашборд Grafana подключается автоматически из `deploy/grafana/`.

Спаны: `POST /v1/chat/completions`, внутри `attempt <upstream>/<model>` на каждую попытку, внутри `upstream <имя>` на HTTP-запрос к модели. Заголовок `traceparent` от клиента принимается и передаётся в модель. Когда трейсы включены, в каждой строке лога о запросе есть `traceId`.

## Make

| Команда | Что делает |
|---|---|
| `make run` | запустить шлюз локально с `config.yaml` |
| `make build` | собрать бинарник в `bin/` |
| `make test` | тесты с покрытием |
| `make test-race` | тесты с race detector |
| `make check` | `go mod tidy -diff`, форматирование, golangci-lint |
| `make format` | применить gofumpt и goimports |
| `make docker` | собрать образ `llm-gateway:dev` |
| `make up` / `make down` | поднять / остановить стек в Docker |
| `make ps` | состояние контейнеров и потребление памяти |
| `make logs` | логи шлюза |
| `make pull MODEL=...` | скачать модель в Ollama из compose |
| `make models` | список моделей и что загружено в видеопамять |
| `make smoke` | 5 коротких запросов к роли small |

## Структура

```text
cmd/llm-gateway/      main, флаги, логи, сигналы, подкоманда healthcheck
internal/config/      загрузка и проверка YAML
internal/server/      HTTP-обработчики, fallback, стриминг, разбор usage
internal/upstream/    HTTP-клиент к апстриму и очередь
internal/metrics/     метрики Prometheus
internal/telemetry/   трейсы OpenTelemetry
deploy/               конфиги шлюза, Prometheus, Alertmanager, Grafana для Docker
compose.yaml          весь стек
```
