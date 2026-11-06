# Команды сборки, запуска и проверки шлюза
GO ?= go

.PHONY: build run test test-race check format

build: ## Собрать бинарник в bin/
	$(GO) build -trimpath -o bin/ ./cmd/llm-gateway

run: ## Запустить шлюз с config.yaml (Ctrl+C - корректная остановка)
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
