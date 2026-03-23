package pipe

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/x3ps/rns-iface-email/internal/envelope"
	"github.com/x3ps/rns-iface-email/internal/transport"
)

type mockSender struct {
	mu       sync.Mutex
	calls    []sendCall
	failNext int // number of times to fail before succeeding
	sendErr  error // if non-nil, always return this error (overrides failNext)
	sendCnt  atomic.Int32
	probeErr error
	probeMu  sync.Mutex
	probeCnt atomic.Int32
}

type sendCall struct {
	to  string
	msg []byte
}

func (m *mockSender) Send(_ context.Context, to string, msg []byte) error {
	m.sendCnt.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		return m.sendErr
	}
	if m.failNext > 0 {
		m.failNext--
		return errors.New("transient SMTP error")
	}
	m.calls = append(m.calls, sendCall{to: to, msg: msg})
	return nil
}

func (m *mockSender) Probe(_ context.Context) error {
	m.probeCnt.Add(1)
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	return m.probeErr
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestHandlePacketSendsEmail(t *testing.T) {
	sender := &mockSender{}
	h := NewHandler(sender, testLogger(), "peer1@test.com", "from@test.com", nil, 0, 0)

	if err := h.HandlePacket(context.Background(), []byte("test-packet")); err != nil {
		t.Fatal(err)
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send call, got %d", len(sender.calls))
	}
	if sender.calls[0].to != "peer1@test.com" {
		t.Errorf("to = %q, want peer1@test.com", sender.calls[0].to)
	}
}

func TestHandlePacketRetriesOnTransientFailure(t *testing.T) {
	sender := &mockSender{failNext: 2}
	h := NewHandler(sender, testLogger(), "peer1@test.com", "from@test.com", nil, 0, 0)

	if err := h.HandlePacket(context.Background(), []byte("retry-packet")); err != nil {
		t.Fatal(err)
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 successful send after retries, got %d", len(sender.calls))
	}
}

func TestHandlePacketLogsOnFinalFailure(t *testing.T) {
	sender := &mockSender{failNext: 100} // always fail
	h := NewHandler(sender, testLogger(), "peer1@test.com", "from@test.com", nil, 0, 0)

	// Should not panic or return error — packet is lost.
	if err := h.HandlePacket(context.Background(), []byte("doomed-packet")); err != nil {
		t.Fatalf("expected nil error on final failure, got %v", err)
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.calls) != 0 {
		t.Errorf("expected 0 successful sends, got %d", len(sender.calls))
	}
}

func TestHandlePacketEncodesCorrectEnvelope(t *testing.T) {
	sender := &mockSender{}
	h := NewHandler(sender, testLogger(), "peer1@test.com", "from@test.com", nil, 0, 0)

	packet := []byte("envelope-roundtrip-test")
	if err := h.HandlePacket(context.Background(), packet); err != nil {
		t.Fatal(err)
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}

	decoded, err := envelope.Decode(sender.calls[0].msg)
	if err != nil {
		t.Fatalf("decode stored envelope: %v", err)
	}
	if string(decoded.Packet) != string(packet) {
		t.Errorf("decoded packet = %q, want %q", decoded.Packet, packet)
	}
}

func TestRecoveryModeTriggered(t *testing.T) {
	t.Parallel()
	sender := &mockSender{failNext: 100}
	var onlineCalls []bool
	var mu sync.Mutex
	setOnline := func(online bool) {
		mu.Lock()
		onlineCalls = append(onlineCalls, online)
		mu.Unlock()
	}

	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com",
		setOnline, 10*time.Millisecond, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = h.HandlePacket(ctx, []byte("fail-pkt"))

	// Wait for setOnline(false) to be called.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := len(onlineCalls)
		mu.Unlock()
		if got >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for setOnline(false)")
		case <-time.After(10 * time.Millisecond):
		}
	}

	mu.Lock()
	if onlineCalls[0] != false {
		t.Errorf("first setOnline call = %v, want false", onlineCalls[0])
	}
	mu.Unlock()

	// Wait for recovery probe to succeed and setOnline(true).
	deadline = time.After(2 * time.Second)
	for {
		mu.Lock()
		got := len(onlineCalls)
		mu.Unlock()
		if got >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for setOnline(true)")
		case <-time.After(10 * time.Millisecond):
		}
	}

	mu.Lock()
	if onlineCalls[1] != true {
		t.Errorf("second setOnline call = %v, want true", onlineCalls[1])
	}
	mu.Unlock()

	cancel()
}

func TestRecoveryProbeRetriesWithBackoff(t *testing.T) {
	t.Parallel()
	sender := &mockSender{failNext: 100, probeErr: errors.New("probe fail")}

	var onlineCalls []bool
	var mu sync.Mutex
	setOnline := func(online bool) {
		mu.Lock()
		onlineCalls = append(onlineCalls, online)
		mu.Unlock()
	}

	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com",
		setOnline, 10*time.Millisecond, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = h.HandlePacket(ctx, []byte("probe-retry-pkt"))

	// Wait for a few probe attempts.
	deadline := time.After(2 * time.Second)
	for sender.probeCnt.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("timed out: only %d probe calls", sender.probeCnt.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Now let probe succeed.
	sender.probeMu.Lock()
	sender.probeErr = nil
	sender.probeMu.Unlock()

	// Wait for setOnline(true).
	deadline = time.After(2 * time.Second)
	for {
		mu.Lock()
		hasTrue := false
		for _, v := range onlineCalls {
			if v {
				hasTrue = true
			}
		}
		mu.Unlock()
		if hasTrue {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for setOnline(true) after probe success")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
}

func TestRecoveryNotDuplicated(t *testing.T) {
	t.Parallel()
	sender := &mockSender{failNext: 100, probeErr: errors.New("probe fail")}

	var onlineFalseCnt atomic.Int32
	setOnline := func(online bool) {
		if !online {
			onlineFalseCnt.Add(1)
		}
	}

	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com",
		setOnline, 50*time.Millisecond, 200*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two consecutive failures.
	_ = h.HandlePacket(ctx, []byte("dup-1"))
	_ = h.HandlePacket(ctx, []byte("dup-2"))

	time.Sleep(100 * time.Millisecond)

	if cnt := onlineFalseCnt.Load(); cnt != 1 {
		t.Errorf("setOnline(false) called %d times, want 1 (CompareAndSwap should prevent duplicate)", cnt)
	}

	cancel()
}

func TestHandlePacketContextCancellation(t *testing.T) {
	t.Parallel()
	sender := &mockSender{failNext: 100}
	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com", nil, 0, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = h.HandlePacket(ctx, []byte("cancel-pkt"))
		close(done)
	}()

	// Cancel quickly — should interrupt the retry backoff.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("HandlePacket did not return after context cancellation")
	}
}

func TestHandlePacketExactlyMaxRetries(t *testing.T) {
	// failNext = maxRetries means all attempts fail — packet should be lost.
	sender := &mockSender{failNext: maxRetries}
	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com", nil, 0, 0)

	err := h.HandlePacket(context.Background(), []byte("max-retry-pkt"))
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.calls) != 0 {
		t.Errorf("expected 0 successful sends (all %d retries exhausted), got %d", maxRetries, len(sender.calls))
	}
}

func TestRecoveryAfterSuccessResetsState(t *testing.T) {
	t.Parallel()
	sender := &mockSender{failNext: 100, probeErr: errors.New("initial fail")}

	var mu sync.Mutex
	var falseCnt, trueCnt int
	setOnline := func(online bool) {
		mu.Lock()
		if online {
			trueCnt++
		} else {
			falseCnt++
		}
		mu.Unlock()
	}

	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com",
		setOnline, 10*time.Millisecond, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First failure triggers recovery.
	_ = h.HandlePacket(ctx, []byte("pkt-1"))

	// Wait for setOnline(false) #1.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := falseCnt
		mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for setOnline(false) #1")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Let probe succeed → setOnline(true) → recovering reset to false.
	sender.probeMu.Lock()
	sender.probeErr = nil
	sender.probeMu.Unlock()

	deadline = time.After(2 * time.Second)
	for {
		mu.Lock()
		n := trueCnt
		mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for setOnline(true)")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Wait for recovering flag to be cleared by defer.
	deadline = time.After(2 * time.Second)
	for h.recovering.Load() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for recovering flag to reset")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Make probe fail again so second recovery doesn't complete quickly.
	sender.probeMu.Lock()
	sender.probeErr = errors.New("fail again")
	sender.probeMu.Unlock()

	// Second failure should start a new recovery cycle.
	_ = h.HandlePacket(ctx, []byte("pkt-2"))

	deadline = time.After(2 * time.Second)
	for {
		mu.Lock()
		n := falseCnt
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for setOnline(false) #2")
		case <-time.After(10 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if falseCnt < 2 {
		t.Errorf("setOnline(false) called %d times, want >= 2 (two recovery cycles)", falseCnt)
	}
}

func TestConcurrentHandlePacket(t *testing.T) {
	t.Parallel()
	sender := &mockSender{failNext: 100, probeErr: errors.New("probe fail")}

	var falseCnt atomic.Int32
	setOnline := func(online bool) {
		if !online {
			falseCnt.Add(1)
		}
	}

	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com",
		setOnline, 50*time.Millisecond, 200*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_ = h.HandlePacket(ctx, []byte("concurrent-pkt"))
		}()
	}
	wg.Wait()

	time.Sleep(100 * time.Millisecond)

	if cnt := falseCnt.Load(); cnt != 1 {
		t.Errorf("setOnline(false) called %d times, want 1 (CompareAndSwap prevents duplicates)", cnt)
	}
}

func TestRecoveryBackoffCap(t *testing.T) {
	t.Parallel()
	sender := &mockSender{failNext: 100, probeErr: errors.New("probe fail")}
	setOnline := func(bool) {}

	// maxRecoveryDelay is 2× recoveryDelay, so the cap takes effect on the third probe.
	recoveryDelay := 20 * time.Millisecond
	maxRecoveryDelay := 2 * recoveryDelay

	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com",
		setOnline, recoveryDelay, maxRecoveryDelay)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = h.HandlePacket(ctx, []byte("backoff-cap-pkt"))

	// With cap at 40ms: probes arrive at ~20ms, ~60ms, ~100ms, ~140ms, ~180ms.
	// Without cap (doubling): 20ms, 60ms, 140ms, 300ms, 620ms...
	// Verify 5 probes happen well within 500ms (uncapped would take ~620ms).
	start := time.Now()
	deadline := time.After(500 * time.Millisecond)
	for sender.probeCnt.Load() < 5 {
		select {
		case <-deadline:
			t.Fatalf("expected 5 probes within 500ms (cap should bound backoff), only got %d after %v",
				sender.probeCnt.Load(), time.Since(start))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestStartupProbeSuccess(t *testing.T) {
	t.Parallel()
	sender := &mockSender{}
	var mu sync.Mutex
	var onlineCalls []bool
	setOnline := func(online bool) {
		mu.Lock()
		onlineCalls = append(onlineCalls, online)
		mu.Unlock()
	}

	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com",
		setOnline, 10*time.Millisecond, 100*time.Millisecond)

	h.StartupProbe(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(onlineCalls) != 1 || onlineCalls[0] != true {
		t.Errorf("onlineCalls = %v, want [true]", onlineCalls)
	}
}

func TestStartupProbeFailureStartsRecovery(t *testing.T) {
	t.Parallel()
	sender := &mockSender{probeErr: errors.New("smtp down")}
	var mu sync.Mutex
	var onlineCalls []bool
	setOnline := func(online bool) {
		mu.Lock()
		onlineCalls = append(onlineCalls, online)
		mu.Unlock()
	}

	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com",
		setOnline, 10*time.Millisecond, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.StartupProbe(ctx)

	// Should have called setOnline(false).
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		hasFalse := len(onlineCalls) > 0 && !onlineCalls[0]
		mu.Unlock()
		if hasFalse {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for setOnline(false)")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Let probe succeed → recovery should call setOnline(true).
	sender.probeMu.Lock()
	sender.probeErr = nil
	sender.probeMu.Unlock()

	deadline = time.After(2 * time.Second)
	for {
		mu.Lock()
		hasTrue := false
		for _, v := range onlineCalls {
			if v {
				hasTrue = true
			}
		}
		mu.Unlock()
		if hasTrue {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for setOnline(true) after recovery")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestStartupProbeAndHandlePacketRecoveryConverge(t *testing.T) {
	t.Parallel()
	sender := &mockSender{failNext: 100, probeErr: errors.New("smtp down")}
	var onlineFalseCnt atomic.Int32
	setOnline := func(online bool) {
		if !online {
			onlineFalseCnt.Add(1)
		}
	}

	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com",
		setOnline, 50*time.Millisecond, 200*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// StartupProbe fails → beginRecovery via CAS.
	h.StartupProbe(ctx)

	// HandlePacket exhausts retries → also calls beginRecovery, but CAS
	// should prevent a second recovery goroutine.
	_ = h.HandlePacket(ctx, []byte("converge-pkt"))

	time.Sleep(100 * time.Millisecond)

	// Exactly one setOnline(false) should have fired (CAS prevents duplicate).
	if cnt := onlineFalseCnt.Load(); cnt != 1 {
		t.Errorf("setOnline(false) called %d times, want 1 (CAS should prevent duplicate recovery)", cnt)
	}
}

func TestHandlePacketEnvelopeEncodeError(t *testing.T) {
	t.Parallel()
	sender := &mockSender{}
	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com", nil, 0, 0)

	// Empty packet triggers encode error.
	err := h.HandlePacket(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected nil return on encode error, got %v", err)
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.calls) != 0 {
		t.Errorf("expected 0 sends after encode error, got %d", len(sender.calls))
	}
}

func TestHandlePacketNoRetryOnDataOutcomeUnknown(t *testing.T) {
	t.Parallel()
	sender := &mockSender{
		sendErr: &transport.ErrDataOutcomeUnknown{Err: errors.New("smtp data close: EOF")},
	}

	var mu sync.Mutex
	var onlineCalls []bool
	setOnline := func(online bool) {
		mu.Lock()
		onlineCalls = append(onlineCalls, online)
		mu.Unlock()
	}

	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com",
		setOnline, 10*time.Millisecond, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := h.HandlePacket(ctx, []byte("ambiguous-pkt"))
	if err != nil {
		t.Fatalf("expected nil return, got %v", err)
	}

	// Exactly 1 Send call — no retry.
	if cnt := sender.sendCnt.Load(); cnt != 1 {
		t.Errorf("Send called %d times, want 1 (no retry on ambiguous outcome)", cnt)
	}

	// Recovery should have been triggered (setOnline(false)).
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		hasFalse := len(onlineCalls) > 0 && !onlineCalls[0]
		mu.Unlock()
		if hasFalse {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for setOnline(false) — recovery not triggered")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestHandlePacketRetriesOnSMTPReject(t *testing.T) {
	t.Parallel()
	// failNext=2 means first 2 calls fail with a regular error, 3rd succeeds.
	sender := &mockSender{failNext: 2}
	h := NewHandler(sender, testLogger(), "peer@test.com", "from@test.com", nil, 0, 0)

	err := h.HandlePacket(context.Background(), []byte("retry-pkt"))
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// Should have retried and eventually succeeded.
	if cnt := sender.sendCnt.Load(); cnt != 3 {
		t.Errorf("Send called %d times, want 3 (2 failures + 1 success)", cnt)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.calls) != 1 {
		t.Errorf("expected 1 successful send, got %d", len(sender.calls))
	}
}
