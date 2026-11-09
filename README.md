# llm-gateway

HTTP-шлюз к языковым моделям с API как у OpenAI. Клиент указывает в поле `model` не конкретную модель, а роль: `small`, `main` или `validator`. Какая модель стоит за ролью, решает конфиг шлюза. Если модель не ответила, шлюз сам пробует следующую. К каждому серверу моделей держит очередь, пишет метрики Prometheus, трейсы OpenTelemetry и логи.

Шлюз и весь стек мониторинга поднимаются одной командой `make up`.

## Место в проекте

В проекте Incident Copilot агенты ходят к моделям только через этот шлюз.

| Репозиторий | Язык | Роль |
|---|---|---|
| [incident-radar](https://github.com/avequa/incident-radar) | Go | читает логи и считает статистику, MCP-сервер для агентов |
| **llm-gateway** (этот) | Go | шлюз к моделям и стек мониторинга |
| [incident-copilot](https://github.com/avequa/incident-copilot) | Python | агенты на LangGraph |

Зачем отдельный шлюз:

- **код агентов не знает про модели.** Чтобы поменять модель для роли, достаточно поправить одну строку в конфиге, агентов трогать не нужно
- **роутинг для экономии.** Простые задачи (выбрать агента, короткий ответ) идут на маленькую модель, разбор логов на основную, проверка на модель другого семейства
- **одна видеокарта.** Очередь пускает к Ollama один запрос за раз, остальные ждут, а не роняют модель
- **видно, что происходит.** По метрикам видно токены, время и ошибки по каждой роли, по трейсу видно, что было с конкретным запросом

## Что внутри

- около 1500 строк на Go и примерно столько же тестов, покрытие 87%
- запасные модели: переход к следующей модели роли при ответе 404, 408, 429, 5xx, таймауте или сетевой ошибке
- потоковые ответы (SSE) отдаются клиенту сразу, как приходят
- лимит `max_tokens` на роль, настройки по умолчанию для роли (`temperature`, `reasoning_effort`)
- ошибки в формате OpenAI, поэтому любые библиотеки для OpenAI работают со шлюзом без изменений
- 8 метрик Prometheus, 6 алертов, дашборд Grafana, трейсы в Jaeger, логи в JSON с `traceId`
- принимает заголовок `traceparent`: трейс агента и трейс шлюза получаются одним трейсом
- образ на distroless, не от root, файловая система только для чтения, все порты только на 127.0.0.1
- в CI линтеры, тесты с `-race`, проверка конфигов Prometheus, Alertmanager и compose, сборка образа

```text
агент ──► llm-gateway ──► очередь ──► Ollama (qwen3:1.7b, qwen3:0.6b, gemma3:1b)
              │
              ├─► /metrics ──► Prometheus ──► Alertmanager
              │                    └────────► Grafana
              └─► трейсы ──► Jaeger
```

## Запуск в Docker

Нужен Docker с поддержкой видеокарты NVIDIA (Docker Desktop с WSL 2 или Docker Engine с NVIDIA Container Toolkit).

```bash
cp .env.example .env            # путь к уже скачанным моделям Ollama и пароль Grafana
make up                         # шлюз, Ollama, Prometheus, Alertmanager, Grafana, Jaeger
make pull MODEL=qwen3:1.7b      # скачать модель, если её ещё нет
make smoke                      # 5 коротких запросов к роли small
```

| Сервис | Адрес |
|---|---|
| шлюз | http://127.0.0.1:4000 |
| Ollama | http://127.0.0.1:11434 |
| Grafana (admin и пароль из `.env`) | http://127.0.0.1:3000 |
| Prometheus | http://127.0.0.1:9090 |
| Alertmanager | http://127.0.0.1:9093 |
| Jaeger | http://127.0.0.1:16686 |

Остановить: `make down`. Посмотреть скачанные модели и что сейчас в видеопамяти: `make models`.

Если Ollama уже стоит на машине как служба, её нужно выключить, иначе две Ollama будут делить видеокарту и порт 11434:

```bash
sudo systemctl disable --now ollama
```

Без Docker (нужны Go 1.26+ и Ollama на `localhost:11434`):

```bash
cp config.example.yaml config.yaml
make run
```

## Запросы

```bash
curl -i localhost:4000/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model": "small", "messages": [{"role": "user", "content": "Скажи одно слово: привет"}]}'
```

В ответе есть заголовки `X-Gateway-Target` (какая модель ответила) и `X-Gateway-Attempts` (с какой попытки).

Из Python работает любой клиент для OpenAI:

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:4000/v1", api_key="unused")
resp = client.chat.completions.create(model="main", messages=[{"role": "user", "content": "привет"}])
```

```python
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(base_url="http://127.0.0.1:4000/v1", api_key="unused", model="main")
```

Тело запроса уходит в модель как есть, меняется только `model`. Поэтому `tools`, `response_format`, `stream` и остальные поля работают так же, как с OpenAI.

| Метод и путь | Что возвращает |
|---|---|
| `POST /v1/chat/completions` | ответ модели, JSON или поток SSE |
| `GET /v1/models` | список ролей |
| `GET /metrics` | метрики Prometheus |
| `GET /healthz` | `ok` |

Ошибки:

| Ситуация | Ответ |
|---|---|
| неизвестная роль | 404 `model_not_found` со списком ролей |
| неправильное тело или `max_tokens` | 400 |
| тело больше 16 MiB | 413 |
| запрос ждал в очереди дольше `queue_timeout` | 429 `upstream_busy`, заголовок `Retry-After: 5` |
| ни одна модель роли не ответила | 502 `upstream_unavailable` или 504 `upstream_timeout` |

Если клиент отключился посреди ответа, запрос к модели отменяется.

## Конфигурация

Роли, которые сейчас стоят в `deploy/gateway.yaml` (модели выбраны бенчмарком в incident-copilot):

```yaml
upstreams:
  ollama:
    base_url: http://ollama:11434/v1
    timeout: 120s
    max_concurrent: 1      # одна видеокарта: один запрос за раз
    queue_timeout: 180s

models:
  small:
    params: {reasoning_effort: none, temperature: 0.1}
    max_tokens: 512
    targets:
      - {upstream: ollama, model: qwen3:1.7b}
      - {upstream: ollama, model: qwen3:0.6b}   # запасная
  main:
    params: {reasoning_effort: none, temperature: 0.2}
    max_tokens: 1024
    targets:
      - {upstream: ollama, model: qwen3:1.7b}
      - {upstream: ollama, model: qwen3:0.6b}
  validator:              # другое семейство моделей, чем у main
    params: {temperature: 0}
    max_tokens: 512
    targets:
      - {upstream: ollama, model: gemma3:1b}
```

| Поле | По умолчанию | Что значит |
|---|---|---|
| `server.addr` | `127.0.0.1:4000` | адрес шлюза |
| `server.shutdown_timeout` | `15s` | сколько ждать активные запросы при остановке |
| `upstreams.<имя>.base_url` | обязательно | адрес OpenAI-совместимого API, заканчивается на `/v1` |
| `upstreams.<имя>.api_key_env` | нет | переменная окружения с ключом, если нужен внешний API |
| `upstreams.<имя>.timeout` | `120s` | сколько ждать начала ответа модели |
| `upstreams.<имя>.max_concurrent` | `4` | сколько запросов одновременно отправлять |
| `upstreams.<имя>.queue_timeout` | `60s` | сколько запрос может ждать в очереди |
| `models.<роль>.targets` | обязательно | модели по порядку: `{upstream, model}` |
| `models.<роль>.params` | нет | значения полей запроса, если клиент их не передал |
| `models.<роль>.max_tokens` | нет | максимум для `max_tokens`, большее значение урезается |

Неизвестные поля в конфиге считаются ошибкой, при запуске выводятся сразу все ошибки. Флаги: `--config`, `--log-format text|json`, `--log-level`. Переменная `OTEL_EXPORTER_OTLP_ENDPOINT` включает трейсы (например, `http://jaeger:4318`).

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

`model` здесь это роль, `target` это модель, которая ответила.

| Алерт | Когда срабатывает |
|---|---|
| LLMGatewayDown | шлюз недоступен 1 минуту |
| LLMHighErrorRate | больше 20% ответов 5xx и 429 за 5 минут |
| LLMSlowResponses | p95 ответа больше 60 с в течение 10 минут |
| LLMSlowFirstToken | p95 до первого токена больше 15 с в течение 10 минут |
| LLMFrequentFallback | больше 5 неудачных попыток одной модели за 15 минут |
| LLMQueueBacklog | в очереди больше 2 запросов 5 минут |

Алерты приходят в Alertmanager, отправка в Telegram подготовлена в `deploy/alertmanager/alertmanager.yml`. Дашборд Grafana подключается сам из `deploy/grafana/`.

Трейс одного запроса: `POST /v1/chat/completions`, внутри `attempt <upstream>/<model>` на каждую попытку, внутри `upstream <имя>` на сам запрос к модели. В каждой строке лога о запросе есть `traceId`, по нему запрос находится в Jaeger.

## Make

| Команда | Что делает |
|---|---|
| `make run` / `make build` | запустить локально / собрать бинарник |
| `make test` / `make test-race` | тесты с покрытием / с race detector |
| `make check` | `go mod tidy -diff`, форматирование, golangci-lint |
| `make up` / `make down` | поднять / остановить стек в Docker |
| `make ps` / `make logs` | состояние контейнеров / логи шлюза |
| `make pull MODEL=...` / `make models` | скачать модель / что скачано и загружено |
| `make smoke` | 5 коротких запросов к роли small |

## Структура

```text
cmd/llm-gateway/      main, флаги, сигналы, подкоманда healthcheck для образа без shell
internal/config/      загрузка и проверка YAML
internal/server/      HTTP-обработчики, запасные модели, стриминг, разбор usage
internal/upstream/    клиент к серверу моделей и очередь
internal/metrics/     метрики Prometheus
internal/telemetry/   трейсы OpenTelemetry
deploy/               конфиги шлюза, Prometheus, Alertmanager, Grafana
compose.yaml          весь стек
```
