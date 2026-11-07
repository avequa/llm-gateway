package upstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/avequa/llm-gateway/internal/config"
)

// TestAcquire_Limit - сверх max_concurrent запрос ждет освобождения слота, а после queue_timeout получает ErrBusy
func TestAcquire_Limit(t *testing.T) {
	c := New("ollama", config.Upstream{BaseURL: "http://localhost", MaxConcurrent: 1, QueueTimeout: 50 * time.Millisecond})

	release, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err = c.Acquire(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Fatalf("waited %v, want at least queue_timeout", waited)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		release()
		release()
	}()
	second, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatalf("slot was not handed over after release: %v", err)
	}
	second()

	third, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatalf("double release broke the limiter: %v", err)
	}
	third()
}

// TestAcquire_Canceled - отмена context прерывает ожидание слота
func TestAcquire_Canceled(t *testing.T) {
	c := New("ollama", config.Upstream{BaseURL: "http://localhost", MaxConcurrent: 1, QueueTimeout: time.Minute})
	release, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline", err)
	}
}
