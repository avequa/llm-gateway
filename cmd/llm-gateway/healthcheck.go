package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"
)

// healthcheckTimeout - сколько ждать ответа /healthz
const healthcheckTimeout = 3 * time.Second

// runHealthcheck - подкоманда healthcheck: GET /healthz и код 0, если шлюз ответил 200;
// нужна для HEALTHCHECK в Docker, потому что в distroless-образе нет curl и wget
func runHealthcheck(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("llm-gateway healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "http://127.0.0.1:4000/healthz", "health endpoint to check")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "unhealthy: %v\n", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "unhealthy: %v\n", err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(stderr, "unhealthy: %s returned %d\n", *url, resp.StatusCode)
		return 1
	}
	return 0
}
