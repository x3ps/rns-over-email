package pipe

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	rnspipe "github.com/x3ps/go-rns-pipe"
)

func TestInjectWithRetryImmediateSuccess(t *testing.T) {
	var called int
	err := InjectWithRetry(context.Background(), []byte("pkt"), func(b []byte) error {
		called++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != 1 {
		t.Errorf("called %d times, want 1", called)
	}
}

func TestInjectWithRetryErrOfflineRetries(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := InjectWithRetry(ctx, []byte("pkt"), func(b []byte) error {
		n := calls.Add(1)
		if n < 3 {
			return rnspipe.ErrOffline
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() < 3 {
		t.Errorf("calls = %d, want >= 3", calls.Load())
	}
}

func TestInjectWithRetryErrNotStartedRetries(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := InjectWithRetry(ctx, []byte("pkt"), func(b []byte) error {
		n := calls.Add(1)
		if n < 2 {
			return rnspipe.ErrNotStarted
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() < 2 {
		t.Errorf("calls = %d, want >= 2", calls.Load())
	}
}

func TestInjectWithRetryNonRetryableError(t *testing.T) {
	permanent := errors.New("permanent failure")
	err := InjectWithRetry(context.Background(), []byte("pkt"), func(b []byte) error {
		return permanent
	})
	if !errors.Is(err, permanent) {
		t.Errorf("got %v, want %v", err, permanent)
	}
}

func TestInjectWithRetryContextCancelledDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := InjectWithRetry(ctx, []byte("pkt"), func(b []byte) error {
		return rnspipe.ErrOffline
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %v, expected prompt return after cancel", elapsed)
	}
}

func TestInjectWithRetryContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := InjectWithRetry(ctx, []byte("pkt"), func(b []byte) error {
		t.Fatal("receive should not be called with cancelled context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}
