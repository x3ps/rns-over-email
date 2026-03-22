package online

import (
	"log/slog"
	"os"
	"sync"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestAggregatorSequence(t *testing.T) {
	// Table-driven sequence test:
	// initial callback(false) → SetIMAP(true) with smtp=true → callback(true)
	// → SetSMTP(false) → callback(false) → SetSMTP(true) → callback(true)
	// → SetIMAP(false) → callback(false)
	var calls []bool
	agg := NewAggregator(func(online bool) {
		calls = append(calls, online)
	}, testLogger())

	// Initial callback(false) already fired.
	if len(calls) != 1 || calls[0] != false {
		t.Fatalf("initial calls = %v, want [false]", calls)
	}

	agg.SetIMAP(true) // smtp=true, imap=true → combined=true
	if len(calls) != 2 || calls[1] != true {
		t.Fatalf("after SetIMAP(true): calls = %v, want [false true]", calls)
	}

	agg.SetSMTP(false) // smtp=false, imap=true → combined=false
	if len(calls) != 3 || calls[2] != false {
		t.Fatalf("after SetSMTP(false): calls = %v, want [false true false]", calls)
	}

	agg.SetSMTP(true) // smtp=true, imap=true → combined=true
	if len(calls) != 4 || calls[3] != true {
		t.Fatalf("after SetSMTP(true): calls = %v, want [false true false true]", calls)
	}

	agg.SetIMAP(false) // smtp=true, imap=false → combined=false
	if len(calls) != 5 || calls[4] != false {
		t.Fatalf("after SetIMAP(false): calls = %v, want [false true false true false]", calls)
	}
}

func TestAggregatorSMTPDownIMAPUp(t *testing.T) {
	var calls []bool
	agg := NewAggregator(func(online bool) {
		calls = append(calls, online)
	}, testLogger())

	agg.SetSMTP(false) // smtp=false, imap=false → combined=false (no transition)
	agg.SetIMAP(true)  // smtp=false, imap=true → combined=false (no transition)

	// Only the initial callback(false) should have fired.
	if len(calls) != 1 {
		t.Errorf("calls = %v, want [false] (SMTP down + IMAP up = offline)", calls)
	}
}

func TestAggregatorIMAPDownSMTPUp(t *testing.T) {
	var calls []bool
	agg := NewAggregator(func(online bool) {
		calls = append(calls, online)
	}, testLogger())

	// smtp starts true, imap starts false → combined=false.
	// SetSMTP(true) is redundant, combined stays false.
	agg.SetSMTP(true)

	if len(calls) != 1 {
		t.Errorf("calls = %v, want [false] (IMAP down + SMTP up = offline)", calls)
	}
}

func TestAggregatorDedup(t *testing.T) {
	var calls []bool
	agg := NewAggregator(func(online bool) {
		calls = append(calls, online)
	}, testLogger())

	// Redundant calls should not trigger additional callbacks.
	agg.SetIMAP(false) // already false
	agg.SetSMTP(true)  // already true
	agg.SetIMAP(false) // still false

	if len(calls) != 1 {
		t.Errorf("calls = %v, want [false] (redundant calls should be deduped)", calls)
	}

	// Now go online.
	agg.SetIMAP(true) // combined=true → transition
	if len(calls) != 2 || calls[1] != true {
		t.Fatalf("calls = %v, want [false true]", calls)
	}

	// Redundant online calls.
	agg.SetIMAP(true)
	agg.SetSMTP(true)
	if len(calls) != 2 {
		t.Errorf("calls = %v, want [false true] (redundant online calls should be deduped)", calls)
	}
}

func TestAggregatorConcurrent(t *testing.T) {
	var mu sync.Mutex
	var calls []bool
	agg := NewAggregator(func(online bool) {
		mu.Lock()
		calls = append(calls, online)
		mu.Unlock()
	}, testLogger())

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			agg.SetIMAP(true)
			agg.SetIMAP(false)
		}()
		go func() {
			defer wg.Done()
			agg.SetSMTP(true)
			agg.SetSMTP(false)
		}()
	}
	wg.Wait()

	// Just verify no panics and calls slice is non-empty (initial callback).
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Error("expected at least the initial callback")
	}

	// Final state should be offline (both toggled to false last).
	_ = agg // no panic = pass
}
