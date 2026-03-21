package imap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/x3ps/rns-iface-email/internal/config"
	"github.com/x3ps/rns-iface-email/internal/envelope"
	"github.com/x3ps/rns-iface-email/internal/inbox"
)

// mockClient implements the Client interface for testing.
type mockClient struct {
	mu          sync.Mutex
	loginErr    error
	selectState MailboxState
	selectErr   error
	fetchMsgs   []fetchMsg
	fetchErr    error
	idleCh      chan struct{} // closed when idle should wake
	idleErr     error
	hasIdle     bool
	noopErr     error
	closeErr    error
	moveErr     error
	deleteErr   error
	idleCalled  atomic.Int32
	noopCalled  atomic.Int32
	fetchCalled atomic.Int32
	deletedUIDs []uint32
	movedUIDs   []uint32
	moveDest    string
}

type fetchMsg struct {
	uid uint32
	raw []byte
}

func (m *mockClient) Login(_, _ string) error { return m.loginErr }

func (m *mockClient) Select(_ string) (MailboxState, error) {
	return m.selectState, m.selectErr
}

func (m *mockClient) FetchSince(lastUID uint32, handler func(uid uint32, raw []byte) error) error {
	m.fetchCalled.Add(1)
	m.mu.Lock()
	msgs := m.fetchMsgs
	m.mu.Unlock()
	for _, msg := range msgs {
		if msg.uid <= lastUID {
			continue
		}
		if err := handler(msg.uid, msg.raw); err != nil {
			return err
		}
	}
	return m.fetchErr
}

func (m *mockClient) Idle(ctx context.Context) error {
	m.idleCalled.Add(1)
	if m.idleCh != nil {
		select {
		case <-m.idleCh:
		case <-ctx.Done():
		}
	} else {
		<-ctx.Done()
	}
	return m.idleErr
}

func (m *mockClient) HasIdle() bool { return m.hasIdle }
func (m *mockClient) Noop() error   { m.noopCalled.Add(1); return m.noopErr }
func (m *mockClient) MoveUIDs(uids []uint32, dest string) error {
	m.mu.Lock()
	m.movedUIDs = append(m.movedUIDs, uids...)
	m.moveDest = dest
	m.mu.Unlock()
	return m.moveErr
}
func (m *mockClient) DeleteUIDs(uids []uint32) error {
	m.mu.Lock()
	m.deletedUIDs = append(m.deletedUIDs, uids...)
	m.mu.Unlock()
	return m.deleteErr
}
func (m *mockClient) Close() error { return m.closeErr }

// seqIdleClient implements Client with a predefined sequence of Idle return values.
// Used to test behavior that depends on per-call Idle outcomes.
type seqIdleClient struct {
	mu          sync.Mutex
	selectState MailboxState
	idleErrs    []error // dequeued one per Idle call; nil means block until ctx
	idleCalled  atomic.Int32
	noopCalled  atomic.Int32
}

func (c *seqIdleClient) Login(_, _ string) error { return nil }
func (c *seqIdleClient) Select(_ string) (MailboxState, error) {
	return c.selectState, nil
}
func (c *seqIdleClient) FetchSince(_ uint32, _ func(uint32, []byte) error) error { return nil }
func (c *seqIdleClient) Idle(ctx context.Context) error {
	c.idleCalled.Add(1)
	c.mu.Lock()
	if len(c.idleErrs) > 0 {
		err := c.idleErrs[0]
		c.idleErrs = c.idleErrs[1:]
		c.mu.Unlock()
		// Return immediately (both error and nil cases); this simulates an idle
		// that either errors or returns because the server's idle window expired.
		return err
	}
	c.mu.Unlock()
	<-ctx.Done()
	return nil
}
func (c *seqIdleClient) HasIdle() bool                       { return true }
func (c *seqIdleClient) Noop() error                         { c.noopCalled.Add(1); return nil }
func (c *seqIdleClient) MoveUIDs(_ []uint32, _ string) error { return nil }
func (c *seqIdleClient) DeleteUIDs(_ []uint32) error         { return nil }
func (c *seqIdleClient) Close() error                        { return nil }

func newTestRepo(t *testing.T) *inbox.JSONRepo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	return inbox.NewJSONRepo(path, slog.Default())
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func makeTestEnvelope(t *testing.T, packet []byte) []byte {
	t.Helper()
	data, _, err := envelope.Encode(envelope.Params{
		From:   "from@test.com",
		To:     "to@test.com",
		Packet: packet,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFetchDecodesAndInjects(t *testing.T) {
	repo := newTestRepo(t)

	var injected [][]byte
	inject := func(_ context.Context, pkt []byte) error {
		injected = append(injected, pkt)
		return nil
	}

	raw := makeTestEnvelope(t, []byte("hello-rns"))
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs:   []fetchMsg{{uid: 1, raw: raw}},
		hasIdle:     false,
	}

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:       "INBOX",
			PollInterval: config.Duration{Duration: time.Hour},
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	lastUID, err := w.fetchAndProcess(ctx, mock, 0, "INBOX", 100)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if lastUID != 1 {
		t.Errorf("lastUID = %d, want 1", lastUID)
	}
	if len(injected) != 1 || string(injected[0]) != "hello-rns" {
		t.Errorf("injected = %v, want [hello-rns]", injected)
	}
}

func TestDecodeFailurePreserved(t *testing.T) {
	repo := newTestRepo(t)

	var injected int
	inject := func(_ context.Context, _ []byte) error {
		injected++
		return nil
	}

	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs:   []fetchMsg{{uid: 5, raw: []byte("not an email")}},
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	lastUID, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if injected != 0 {
		t.Errorf("expected 0 injections, got %d", injected)
	}
	if lastUID != 0 {
		t.Errorf("lastUID = %d, want 0 (should NOT advance past decode failure)", lastUID)
	}

	cp, err := repo.GetCheckpoint(context.Background(), "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if cp != 0 {
		t.Errorf("checkpoint = %d, want 0 (decode failure should NOT advance checkpoint)", cp)
	}
}

func TestInjectFailureReturnsError(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(_ context.Context, _ []byte) error {
		return errors.New("inject broken")
	}

	raw := makeTestEnvelope(t, []byte("inject-fail"))
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs:   []fetchMsg{{uid: 3, raw: raw}},
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	_, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err == nil {
		t.Fatal("expected error from inject failure, got nil")
	}

	cp, cpErr := repo.GetCheckpoint(context.Background(), "INBOX", 100)
	if cpErr != nil {
		t.Fatal(cpErr)
	}
	if cp != 0 {
		t.Errorf("checkpoint = %d, want 0 (should not advance on inject failure)", cp)
	}
}

func TestInjectFailureRetryFromCheckpoint(t *testing.T) {
	repo := newTestRepo(t)

	raw := makeTestEnvelope(t, []byte("retry-pkt"))

	var mu sync.Mutex
	var injected [][]byte
	callCount := 0
	inject := func(_ context.Context, pkt []byte) error {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		if callCount == 1 {
			return errors.New("transient failure")
		}
		injected = append(injected, pkt)
		return nil
	}

	sessionCount := 0
	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:            "INBOX",
			PollInterval:      config.Duration{Duration: time.Hour},
			ReconnectDelay:    1,
			MaxReconnectDelay: 1,
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}
	w.dial = func(ctx context.Context, _ func()) (Client, error) {
		sessionCount++
		mock := &mockClient{
			selectState: MailboxState{UIDValidity: 100},
			fetchMsgs:   []fetchMsg{{uid: 5, raw: raw}},
			hasIdle:     false,
		}
		return mock, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	deadline := time.After(15 * time.Second)
	for {
		mu.Lock()
		got := len(injected)
		mu.Unlock()
		if got > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("timed out waiting for retry")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if sessionCount < 2 {
		t.Errorf("sessionCount = %d, want >= 2", sessionCount)
	}
	mu.Lock()
	if len(injected) != 1 || string(injected[0]) != "retry-pkt" {
		t.Errorf("injected = %v, want [retry-pkt]", injected)
	}
	mu.Unlock()
}

func TestInjectContextCancellation(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(ctx context.Context, _ []byte) error {
		<-ctx.Done()
		return ctx.Err()
	}

	raw := makeTestEnvelope(t, []byte("cancel-pkt"))
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs:   []fetchMsg{{uid: 1, raw: raw}},
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := w.fetchAndProcess(ctx, mock, 0, "INBOX", 100)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected context error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fetchAndProcess did not return after context cancellation")
	}
}

func TestPollLoopTriggersFetch(t *testing.T) {
	repo := newTestRepo(t)

	var injected int
	inject := func(_ context.Context, _ []byte) error {
		injected++
		return nil
	}

	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		hasIdle:     false,
	}

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:       "INBOX",
			PollInterval: config.Duration{Duration: 50 * time.Millisecond},
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.pollLoop(ctx, mock, 0, "INBOX", 100)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if mock.noopCalled.Load() == 0 {
		t.Error("expected at least one noop call")
	}
	if mock.fetchCalled.Load() == 0 {
		t.Error("expected at least one fetch call")
	}
}

func TestUIDValidityChange(t *testing.T) {
	repo := newTestRepo(t)

	if err := repo.AdvanceCheckpoint(context.Background(), "INBOX", 100, 50); err != nil {
		t.Fatal(err)
	}

	uid, err := repo.GetCheckpoint(context.Background(), "INBOX", 200)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 0 {
		t.Errorf("expected 0 for new uidvalidity, got %d", uid)
	}
}

func TestReconnectBackoff(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(_ context.Context, _ []byte) error { return nil }

	dialCount := 0
	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:            "INBOX",
			PollInterval:      config.Duration{Duration: time.Hour},
			ReconnectDelay:    1, // 1s base — small enough for the test timeout
			MaxReconnectDelay: 4, // 4s cap
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}
	w.dial = func(ctx context.Context, _ func()) (Client, error) {
		dialCount++
		return nil, errors.New("connection refused")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_ = w.Run(ctx)
	if dialCount < 2 {
		t.Errorf("dialCount = %d, want >= 2 (need multiple attempts to verify backoff)", dialCount)
	}
}

func TestIdleExistsNotificationTriggersFetch(t *testing.T) {
	repo := newTestRepo(t)

	var injected [][]byte
	inject := func(_ context.Context, pkt []byte) error {
		injected = append(injected, pkt)
		return nil
	}

	raw := makeTestEnvelope(t, []byte("idle-msg"))
	existsCh := make(chan struct{}, 1)
	idleCh := make(chan struct{})

	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		hasIdle:     true,
		idleCh:      idleCh,
		fetchMsgs:   []fetchMsg{{uid: 1, raw: raw}},
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.idleLoop(ctx, mock, 0, "INBOX", 100, existsCh)
	}()

	existsCh <- struct{}{}
	close(idleCh)

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if mock.fetchCalled.Load() == 0 {
		t.Error("expected fetch after EXISTS notification")
	}
}

func TestShutdownDuringInjectRetry(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(ctx context.Context, _ []byte) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
				return errors.New("offline")
			}
		}
	}

	raw := makeTestEnvelope(t, []byte("shutdown-pkt"))
	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:       "INBOX",
			PollInterval: config.Duration{Duration: time.Hour},
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}
	w.dial = func(ctx context.Context, _ func()) (Client, error) {
		mock := &mockClient{
			selectState: MailboxState{UIDValidity: 100},
			fetchMsgs:   []fetchMsg{{uid: 1, raw: raw}},
			hasIdle:     false,
		}
		return mock, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not exit within timeout after cancellation")
	}
}

func TestIdleErrorFallbackToPoll(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(_ context.Context, _ []byte) error { return nil }

	idleCh := make(chan struct{})
	close(idleCh)
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		hasIdle:     true,
		idleCh:      idleCh,
		idleErr:     errors.New("idle failed"),
	}

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:       "INBOX",
			PollInterval: config.Duration{Duration: 50 * time.Millisecond},
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.idleLoop(ctx, mock, 0, "INBOX", 100, make(chan struct{}, 1))
	}()

	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	if mock.idleCalled.Load() < 3 {
		t.Errorf("idle called %d times, want >= 3", mock.idleCalled.Load())
	}
	if mock.noopCalled.Load() == 0 {
		t.Error("expected noop calls after poll fallback")
	}
}

func TestBackoffResetAfterSuccess(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(_ context.Context, _ []byte) error { return nil }

	sessionCount := 0
	var mu sync.Mutex
	var dialTimes []time.Time
	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:            "INBOX",
			PollInterval:      config.Duration{Duration: 50 * time.Millisecond},
			ReconnectDelay:    1, // 1s base
			MaxReconnectDelay: 4, // 4s cap
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}
	w.dial = func(ctx context.Context, _ func()) (Client, error) {
		mu.Lock()
		sessionCount++
		n := sessionCount
		dialTimes = append(dialTimes, time.Now())
		mu.Unlock()
		if n <= 2 {
			return nil, fmt.Errorf("dial: connection refused")
		}
		return &mockClient{
			selectState: MailboxState{UIDValidity: 100},
			hasIdle:     false,
			noopErr:     errors.New("connection reset"),
		}, nil
	}

	// With ReconnectDelay=1s, MaxReconnectDelay=4s:
	//   session 1 (dial error): t=0, sleep 1s, backoff→2s
	//   session 2 (dial error): t=1s, sleep 2s, backoff→4s
	//   session 3 (success→noop err): t=3s, sleep 4s, backoff→1s (reset: non-dial)
	//   session 4 (success→noop err): t=7s, sleep 1s, backoff stays 1s
	//   session 5: t=8s
	// We verify the reset by checking the gap between sessions 4 and 5 is ~1s.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	deadline := time.After(14 * time.Second)
	for {
		mu.Lock()
		n := sessionCount
		mu.Unlock()
		if n >= 5 {
			break
		}
		select {
		case <-deadline:
			cancel()
			mu.Lock()
			t.Fatalf("timed out: only got %d sessions", sessionCount)
			mu.Unlock()
		case <-time.After(100 * time.Millisecond):
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(dialTimes) >= 5 {
		gap := dialTimes[4].Sub(dialTimes[3])
		if gap > 3*time.Second {
			t.Errorf("gap between session 4 and 5 = %v, want ~1s (backoff should reset after non-dial error)", gap)
		}
	}
}

func TestCleanupDeleteAfterProcess(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(_ context.Context, _ []byte) error { return nil }

	raw := makeTestEnvelope(t, []byte("cleanup-del"))
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs:   []fetchMsg{{uid: 7, raw: raw}},
	}

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:  "INBOX",
			Cleanup: config.CleanupConfig{Mode: "delete"},
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	_, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.deletedUIDs) != 1 || mock.deletedUIDs[0] != 7 {
		t.Errorf("deletedUIDs = %v, want [7]", mock.deletedUIDs)
	}
}

func TestCleanupMoveAfterProcess(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(_ context.Context, _ []byte) error { return nil }

	raw := makeTestEnvelope(t, []byte("cleanup-mv"))
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs:   []fetchMsg{{uid: 3, raw: raw}},
	}

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:  "INBOX",
			Cleanup: config.CleanupConfig{Mode: "move", TargetFolder: "Archive"},
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	_, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.movedUIDs) != 1 || mock.movedUIDs[0] != 3 {
		t.Errorf("movedUIDs = %v, want [3]", mock.movedUIDs)
	}
	if mock.moveDest != "Archive" {
		t.Errorf("moveDest = %q, want Archive", mock.moveDest)
	}
}

func TestCleanupFailureNonFatal(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(_ context.Context, _ []byte) error { return nil }

	raw := makeTestEnvelope(t, []byte("cleanup-fail"))
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs:   []fetchMsg{{uid: 2, raw: raw}},
		deleteErr:   errors.New("cleanup exploded"),
	}

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:  "INBOX",
			Cleanup: config.CleanupConfig{Mode: "delete"},
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	lastUID, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err != nil {
		t.Fatal("cleanup failure should not propagate:", err)
	}
	if lastUID != 2 {
		t.Errorf("lastUID = %d, want 2", lastUID)
	}
}

func TestCleanupNoneByDefault(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(_ context.Context, _ []byte) error { return nil }

	raw := makeTestEnvelope(t, []byte("no-cleanup"))
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs:   []fetchMsg{{uid: 1, raw: raw}},
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	_, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.deletedUIDs) != 0 {
		t.Errorf("expected no deletions, got %v", mock.deletedUIDs)
	}
	if len(mock.movedUIDs) != 0 {
		t.Errorf("expected no moves, got %v", mock.movedUIDs)
	}
}

func TestDecodeFailureCapsCheckpoint(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(_ context.Context, _ []byte) error { return nil }

	raw5 := makeTestEnvelope(t, []byte("pkt5"))
	raw7 := makeTestEnvelope(t, []byte("pkt7"))

	// UIDs [5(ok), 6(decode fail), 7(ok)] → checkpoint must be 5, not 7.
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs: []fetchMsg{
			{uid: 5, raw: raw5},
			{uid: 6, raw: []byte("not a valid email")},
			{uid: 7, raw: raw7},
		},
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	lastUID, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lastUID != 5 {
		t.Errorf("lastUID = %d, want 5 (should not advance past decode failure at UID 6)", lastUID)
	}

	cp, err := repo.GetCheckpoint(context.Background(), "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if cp != 5 {
		t.Errorf("checkpoint = %d, want 5", cp)
	}
}

func TestOutOfOrderUIDs(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(_ context.Context, _ []byte) error { return nil }

	raw1 := makeTestEnvelope(t, []byte("pkt1"))
	raw2 := makeTestEnvelope(t, []byte("pkt2"))
	raw3 := makeTestEnvelope(t, []byte("pkt3"))

	// Server returns UIDs out of order.
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs: []fetchMsg{
			{uid: 10, raw: raw1},
			{uid: 5, raw: raw2},
			{uid: 7, raw: raw3},
		},
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	lastUID, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lastUID != 10 {
		t.Errorf("lastUID = %d, want 10", lastUID)
	}

	cp, err := repo.GetCheckpoint(context.Background(), "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if cp != 10 {
		t.Errorf("checkpoint = %d, want 10", cp)
	}
}

func TestStuckCheckpointLogsError(t *testing.T) {
	repo := newTestRepo(t)

	inject := func(_ context.Context, _ []byte) error { return nil }

	// UID 3 is permanently corrupt; no processable messages before it.
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs:   []fetchMsg{{uid: 3, raw: []byte("not a valid email at all")}},
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: logger,
	}

	lastUID, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lastUID != 0 {
		t.Errorf("lastUID = %d, want 0", lastUID)
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "checkpoint stuck") {
		t.Errorf("expected 'checkpoint stuck' error log, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "stuck_uid=3") {
		t.Errorf("expected stuck_uid=3 in log, got: %s", logOutput)
	}
}

func TestRunSessionLoginFailure(t *testing.T) {
	repo := newTestRepo(t)
	inject := func(_ context.Context, _ []byte) error { return nil }

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:            "INBOX",
			PollInterval:      config.Duration{Duration: time.Hour},
			ReconnectDelay:    1,
			MaxReconnectDelay: 1,
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	dialCount := 0
	w.dial = func(ctx context.Context, _ func()) (Client, error) {
		dialCount++
		return &mockClient{
			loginErr:    errors.New("auth failed"),
			selectState: MailboxState{UIDValidity: 100},
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = w.Run(ctx)

	if dialCount < 2 {
		t.Errorf("dialCount = %d, want >= 2 (login failure should cause reconnect)", dialCount)
	}
}

func TestRunSessionSelectFailure(t *testing.T) {
	repo := newTestRepo(t)
	inject := func(_ context.Context, _ []byte) error { return nil }

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:            "INBOX",
			PollInterval:      config.Duration{Duration: time.Hour},
			ReconnectDelay:    1,
			MaxReconnectDelay: 1,
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	dialCount := 0
	w.dial = func(ctx context.Context, _ func()) (Client, error) {
		dialCount++
		return &mockClient{
			selectErr: errors.New("no such mailbox"),
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = w.Run(ctx)

	if dialCount < 2 {
		t.Errorf("dialCount = %d, want >= 2 (select failure should cause reconnect)", dialCount)
	}
}

func TestSetOnlineCalledOnSessionEstablished(t *testing.T) {
	repo := newTestRepo(t)
	inject := func(_ context.Context, _ []byte) error { return nil }

	var onlineCalls []bool
	var mu sync.Mutex

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:            "INBOX",
			PollInterval:      config.Duration{Duration: 50 * time.Millisecond},
			ReconnectDelay:    1,
			MaxReconnectDelay: 1,
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
		setOnline: func(online bool) {
			mu.Lock()
			onlineCalls = append(onlineCalls, online)
			mu.Unlock()
		},
	}
	w.dial = func(ctx context.Context, _ func()) (Client, error) {
		return &mockClient{
			selectState: MailboxState{UIDValidity: 100},
			hasIdle:     false,
			noopErr:     errors.New("connection reset"), // end session after one poll
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Wait for at least one true+false pair.
	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		got := len(onlineCalls)
		mu.Unlock()
		if got >= 2 {
			break
		}
		select {
		case <-deadline:
			cancel()
			mu.Lock()
			t.Fatalf("timed out: onlineCalls = %v", onlineCalls)
			mu.Unlock()
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	// Should have true (session established) followed by false (session ended).
	foundTrue := false
	foundFalseAfterTrue := false
	for _, v := range onlineCalls {
		if v {
			foundTrue = true
		} else if foundTrue {
			foundFalseAfterTrue = true
		}
	}
	if !foundTrue {
		t.Error("setOnline(true) never called")
	}
	if !foundFalseAfterTrue {
		t.Error("setOnline(false) never called after setOnline(true)")
	}
}

func TestEmptyMailbox(t *testing.T) {
	repo := newTestRepo(t)

	var injected int
	inject := func(_ context.Context, _ []byte) error {
		injected++
		return nil
	}

	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs:   nil, // empty
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	lastUID, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lastUID != 0 {
		t.Errorf("lastUID = %d, want 0", lastUID)
	}
	if injected != 0 {
		t.Errorf("expected 0 injections, got %d", injected)
	}
}

func TestFetchErrorPropagates(t *testing.T) {
	repo := newTestRepo(t)
	inject := func(_ context.Context, _ []byte) error { return nil }

	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchErr:    errors.New("network error during fetch"),
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	_, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err == nil {
		t.Fatal("expected error from FetchSince, got nil")
	}
	if !strings.Contains(err.Error(), "network error during fetch") {
		t.Errorf("error = %q, want substring 'network error during fetch'", err.Error())
	}
}

func TestIdleTimeoutRestart(t *testing.T) {
	// Verify that when idle returns nil (e.g. 25-min restart timeout), fetchAndProcess is called.
	repo := newTestRepo(t)
	inject := func(_ context.Context, _ []byte) error { return nil }

	// Closing idleCh makes Idle return nil immediately, simulating a timeout/restart.
	idleCh := make(chan struct{})
	close(idleCh)

	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		hasIdle:     true,
		idleCh:      idleCh,
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.idleLoop(ctx, mock, 0, "INBOX", 100, make(chan struct{}, 1))
	}()

	// Each nil idle return triggers a fetch; verify at least 2 occur.
	deadline := time.After(3 * time.Second)
	for mock.fetchCalled.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out: fetchCalled=%d (idle timeout should trigger fetch)", mock.fetchCalled.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestMaxReconnectDelayIsCapped(t *testing.T) {
	repo := newTestRepo(t)
	inject := func(_ context.Context, _ []byte) error { return nil }

	var mu sync.Mutex
	var dialTimes []time.Time

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:            "INBOX",
			PollInterval:      config.Duration{Duration: time.Hour},
			ReconnectDelay:    1,
			MaxReconnectDelay: 2, // cap at 2s
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}
	w.dial = func(ctx context.Context, _ func()) (Client, error) {
		mu.Lock()
		dialTimes = append(dialTimes, time.Now())
		mu.Unlock()
		return nil, errors.New("connection refused")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Wait for at least 4 dial attempts (1s + 2s + 2s = 5s of delays before 4th dial).
	deadline := time.After(9 * time.Second)
	for {
		mu.Lock()
		n := len(dialTimes)
		mu.Unlock()
		if n >= 4 {
			break
		}
		select {
		case <-deadline:
			cancel()
			mu.Lock()
			t.Fatalf("only got %d dials within timeout", len(dialTimes))
			mu.Unlock()
		case <-time.After(100 * time.Millisecond):
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(dialTimes) >= 4 {
		// Gap between 3rd and 4th dial should be ~2s (cap), not ~4s (uncapped doubling).
		gap := dialTimes[3].Sub(dialTimes[2])
		if gap > 4*time.Second {
			t.Errorf("gap between dial 3 and 4 = %v, want <= 4s (cap at 2s)", gap)
		}
	}
}

func TestDecodeFailureAndInjectFailureInterleaved(t *testing.T) {
	// Verify ceilingUID = min(failedUID, decodeFailedUID) when both are set.
	// UIDs: 2(ok), 3(corrupt), 4(ok), 5(inject-fail)
	// → processedUIDs=[2,4], decodeFailedUID=3, failedUID=5
	// → ceilingUID=min(5,3)=3 → safeUID=2 (UID 4 is above ceiling)
	repo := newTestRepo(t)

	raw2 := makeTestEnvelope(t, []byte("pkt2"))
	raw4 := makeTestEnvelope(t, []byte("pkt4"))
	raw5 := makeTestEnvelope(t, []byte("pkt5"))

	injectCount := 0
	inject := func(_ context.Context, _ []byte) error {
		injectCount++
		if injectCount == 3 { // third inject call is UID 5
			return errors.New("inject failed at uid 5")
		}
		return nil
	}

	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs: []fetchMsg{
			{uid: 2, raw: raw2},
			{uid: 3, raw: []byte("corrupt bytes")},
			{uid: 4, raw: raw4},
			{uid: 5, raw: raw5},
		},
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	lastUID, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err == nil {
		t.Fatal("expected error from inject failure at UID 5")
	}
	if lastUID != 2 {
		t.Errorf("lastUID = %d, want 2 (ceiling=min(5,3)=3, only UID 2 < ceiling)", lastUID)
	}

	cp, cpErr := repo.GetCheckpoint(context.Background(), "INBOX", 100)
	if cpErr != nil {
		t.Fatal(cpErr)
	}
	if cp != 2 {
		t.Errorf("checkpoint = %d, want 2", cp)
	}
}

func TestCleanupNotCalledOnInjectFailure(t *testing.T) {
	// When inject fails, err != nil, so the cleanup guard (err == nil) prevents
	// Delete/MoveUIDs from being called even if some UIDs were processed.
	repo := newTestRepo(t)

	raw := makeTestEnvelope(t, []byte("fail-pkt"))
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs:   []fetchMsg{{uid: 1, raw: raw}},
	}

	inject := func(_ context.Context, _ []byte) error {
		return errors.New("inject failed")
	}

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:  "INBOX",
			Cleanup: config.CleanupConfig{Mode: "delete"},
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	_, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err == nil {
		t.Fatal("expected error from inject failure")
	}
	if len(mock.deletedUIDs) != 0 {
		t.Errorf("deletedUIDs = %v, want [] (cleanup must not run when inject fails)", mock.deletedUIDs)
	}
}

func TestRunReturnsNilOnContextCancel(t *testing.T) {
	repo := newTestRepo(t)
	inject := func(_ context.Context, _ []byte) error { return nil }

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:            "INBOX",
			PollInterval:      config.Duration{Duration: time.Hour},
			ReconnectDelay:    1,
			MaxReconnectDelay: 1,
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}
	w.dial = func(ctx context.Context, _ func()) (Client, error) {
		return &mockClient{
			selectState: MailboxState{UIDValidity: 100},
			hasIdle:     false,
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run even starts

	err := w.Run(ctx)
	if err != nil {
		t.Errorf("Run returned %v, want nil on context cancel", err)
	}
}

func TestIdleErrorCountResets(t *testing.T) {
	// After a successful idle (error count resets to 0), subsequent errors must
	// not count toward the accumulated total from before the reset.
	// Sequence: error, error, nil (success → reset), error, error.
	// With maxIdleErrors=3, 2 errors before reset + 2 after = 4 errors total,
	// but the reset means the second group starts from 0, so no fallback to poll.
	repo := newTestRepo(t)
	inject := func(_ context.Context, _ []byte) error { return nil }

	qi := &seqIdleClient{
		selectState: MailboxState{UIDValidity: 100},
		idleErrs: []error{
			errors.New("err1"),
			errors.New("err2"),
			nil, // success: resets idleErrors to 0
			errors.New("err3"),
			errors.New("err4"),
		},
	}

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:       "INBOX",
			PollInterval: config.Duration{Duration: 50 * time.Millisecond},
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.idleLoop(ctx, qi, 0, "INBOX", 100, make(chan struct{}, 1))
	}()

	// Wait for all queued responses to be consumed.
	deadline := time.After(5 * time.Second)
	for {
		qi.mu.Lock()
		remaining := len(qi.idleErrs)
		qi.mu.Unlock()
		if remaining == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for idle responses to be consumed")
		case <-time.After(20 * time.Millisecond):
		}
	}

	time.Sleep(50 * time.Millisecond) // let the loop process the last response

	// If the count had NOT been reset, errors 3+4 would push total past the threshold,
	// causing fallback to poll (noopCalled > 0). With reset it stays in idle mode.
	if qi.noopCalled.Load() > 0 {
		t.Errorf("noopCalled = %d: idle error count should have reset, preventing premature poll fallback",
			qi.noopCalled.Load())
	}
	cancel()
	<-done
}

func TestPollLoopNegativeInterval(t *testing.T) {
	// A zero or negative PollInterval must fall back to 60 seconds to prevent CPU spin.
	repo := newTestRepo(t)
	inject := func(_ context.Context, _ []byte) error { return nil }

	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		hasIdle:     false,
	}

	w := &Worker{
		cfg: config.IMAPConfig{
			Folder:       "INBOX",
			PollInterval: config.Duration{Duration: 0}, // zero → must use 60s fallback
		},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.pollLoop(ctx, mock, 0, "INBOX", 100)
	}()

	// With 60s interval, no noop should fire within 100ms.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if mock.noopCalled.Load() != 0 {
		t.Errorf("noopCalled = %d, want 0 (60s fallback interval should not fire in 100ms)",
			mock.noopCalled.Load())
	}
}

func TestOutOfOrderWithFailure(t *testing.T) {
	repo := newTestRepo(t)

	callCount := 0
	inject := func(_ context.Context, _ []byte) error {
		callCount++
		if callCount == 2 { // UID 5 (second message delivered) fails
			return errors.New("inject failed")
		}
		return nil
	}

	raw1 := makeTestEnvelope(t, []byte("pkt1"))
	raw2 := makeTestEnvelope(t, []byte("pkt2"))
	raw3 := makeTestEnvelope(t, []byte("pkt3"))

	// Server returns UIDs [10, 5, 7]. UID 10 succeeds, UID 5 fails.
	mock := &mockClient{
		selectState: MailboxState{UIDValidity: 100},
		fetchMsgs: []fetchMsg{
			{uid: 10, raw: raw1},
			{uid: 5, raw: raw2},
			{uid: 7, raw: raw3},
		},
	}

	w := &Worker{
		cfg:    config.IMAPConfig{Folder: "INBOX"},
		repo:   repo,
		inject: inject,
		logger: testLogger(),
	}

	lastUID, err := w.fetchAndProcess(context.Background(), mock, 0, "INBOX", 100)
	if err == nil {
		t.Fatal("expected error from inject failure")
	}
	if lastUID != 0 {
		t.Errorf("lastUID = %d, want 0 (checkpoint should not advance past failure)", lastUID)
	}

	cp, err := repo.GetCheckpoint(context.Background(), "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if cp != 0 {
		t.Errorf("checkpoint = %d, want 0 (UID 10 succeeded but is above failed UID 5)", cp)
	}
}
