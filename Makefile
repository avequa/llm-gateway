# Команды сборки, запуска и проверки шлюза
GO ?= go
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build run test test-race check format docker up down logs smoke ps pull models

build: ## Собрать бинарник в bin/
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/llm-gateway

run: ## Запустить шлюз локально с config.yaml (Ctrl+C - корректная остановка)
	$(GO) run ./cmd/llm-gateway --config config.yaml

test: ## Запустить тесты с покрытием
	$(GO) test -cover ./...

test-race: ## Запустить тесты с race detector (нужен CGO)
	$(GO) test -race ./...

format: ## Применить gofumpt и goimports
	$(GO) tool golangci-lint fmt

check: ## Проверить go.mod, форматирование и код линтерами
	$(GO) mod tidy -diff
	$(GO) tool golangci-lint fmt --diff
	$(GO) tool golangci-lint run

docker: ## Собрать Docker-образ llm-gateway:$(VERSION)
	docker build --build-arg VERSION=$(VERSION) -t llm-gateway:$(VERSION) .

up: ## Поднять шлюз, Ollama на GPU, Prometheus, Alertmanager, Grafana и Jaeger
	VERSION=$(VERSION) docker compose up -d --build

down: ## Остановить стек (данные Prometheus и Grafana сохраняются в volumes)
	docker compose down

ps: ## Состояние контейнеров и потребление памяти
	docker compose ps
	docker stats --no-stream --format "table {{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}"

pull: ## Скачать модель в Ollama из compose: make pull MODEL=qwen3:1.7b
	docker compose exec ollama ollama pull $(MODEL)

models: ## Модели в Ollama и какие сейчас загружены в видеопамять (PROCESSOR должен быть 100% GPU)
	docker compose exec ollama ollama list
	docker compose exec ollama ollama ps

logs: ## Логи шлюза в реальном времени
	docker compose logs -f gateway

smoke: ## Пять коротких запросов к роли small, чтобы увидеть метрики и трейсы (max_tokens 16, видеокарту не грузит)
	@for i in 1 2 3 4 5; do \
		curl -s -o /dev/null -w "small: %{http_code} %{time_total}s\n" http://127.0.0.1:4000/v1/chat/completions \
			-H 'Content-Type: application/json' \
			-d '{"model":"small","max_tokens":16,"messages":[{"role":"user","content":"Ответь одним словом: ок"}]}'; \
	done
