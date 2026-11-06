package server

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer - потокобезопасный буфер для логов из горутины сервера
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write - пишет в буфер под мьютексом
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String - содержимое буфера под мьютексом
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// listen - listener на свободном локальном порту
func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestRun_GracefulShutdown - после отмены context активный запрос дорабатывает, новые не принимаются, Run возвращает nil
func TestRun_GracefulShutdown(t *testing.T) {
	listener := listen(t)
	started := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	})

	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, listener, slow, 5*time.Second, slog.New(slog.NewTextHandler(&logs, nil))) }()

	type result struct {
		body string
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+listener.Addr().String(), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		resCh <- result{body: b.String()}
	}()

	<-started
	cancel()

	if res := <-resCh; res.err != nil || res.body != "done" {
		t.Fatalf("active request: body = %q, err = %v, want it to finish", res.body, res.err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if !strings.Contains(logs.String(), "gateway stopped") {
		t.Fatalf("no shutdown log:\n%s", logs.String())
	}
}

// TestRun_ShutdownTimeout - если запрос не успевает за shutdownTimeout, соединения закрываются принудительно
func TestRun_ShutdownTimeout(t *testing.T) {
	listener := listen(t)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	stuck := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	})

	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, listener, stuck, 50*time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil)))
	}()

	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+listener.Addr().String(), nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after shutdown timeout")
	}
	if !strings.Contains(logs.String(), "graceful shutdown timed out") {
		t.Fatalf("no forced shutdown log:\n%s", logs.String())
	}
}

// TestRun_ClosedListener - если listener закрыт до старта, Run сразу возвращает ошибку
func TestRun_ClosedListener(t *testing.T) {
	listener := listen(t)
	_ = listener.Close()

	err := Run(context.Background(), listener, http.NotFoundHandler(), time.Second, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err == nil {
		t.Fatal("expected error")
	}
}
