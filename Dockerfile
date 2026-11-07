# Образ шлюза: статический бинарник на distroless, запуск от непривилегированного пользователя
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/llm-gateway ./cmd/llm-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llm-gateway /usr/local/bin/llm-gateway
USER nonroot:nonroot
EXPOSE 4000
HEALTHCHECK --interval=15s --timeout=5s --start-period=5s --retries=3 CMD ["/usr/local/bin/llm-gateway", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/llm-gateway"]
CMD ["--config", "/etc/llm-gateway/config.yaml", "--log-format", "json"]
