package internal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/x3ps/rns-iface-email/internal/config"
	"github.com/x3ps/rns-iface-email/internal/envelope"
	imapworker "github.com/x3ps/rns-iface-email/internal/imap"
	"github.com/x3ps/rns-iface-email/internal/inbox"
	"github.com/x3ps/rns-iface-email/internal/pipe"
	"github.com/x3ps/rns-iface-email/internal/transport"
)

// --- Embedded SMTP test server ---

type e2eMessage struct {
	From string
	To   string
	Data []byte
}

type e2eBackend struct {
	mu       sync.Mutex
	messages []e2eMessage
	failNext atomic.Bool
}

func (b *e2eBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &e2eSession{backend: b}, nil
}

func (b *e2eBackend) Messages() []e2eMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]e2eMessage, len(b.messages))
	copy(cp, b.messages)
	return cp
}

type e2eSession struct {
	backend *e2eBackend
	from    string
	to      string
}

func (s *e2eSession) AuthMechanisms() []string { return []string{"PLAIN"} }

func (s *e2eSession) Auth(mech string) (sasl.Server, error) {
	if mech != "PLAIN" {
		return nil, fmt.Errorf("unsupported: %s", mech)
	}
	return sasl.NewPlainServer(func(_, username, password string) error {
		if username != "testuser" || password != "testpass" {
			return fmt.Errorf("bad creds")
		}
		return nil
	}), nil
}

func (s *e2eSession) Mail(from string, _ *smtp.MailOptions) error { s.from = from; return nil }
func (s *e2eSession) Rcpt(to string, _ *smtp.RcptOptions) error  { s.to = to; return nil }

func (s *e2eSession) Data(r io.Reader) error {
	if s.backend.failNext.CompareAndSwap(true, false) {
		return fmt.Errorf("simulated SMTP failure")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	s.backend.messages = append(s.backend.messages, e2eMessage{
		From: s.from,
		To:   s.to,
		Data: data,
	})
	return nil
}

func (s *e2eSession) Reset()        {}
func (s *e2eSession) Logout() error { return nil }

func startE2ESMTPServer(t *testing.T, backend *e2eBackend) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := smtp.NewServer(backend)
	srv.Domain = "localhost"
	srv.AllowInsecureAuth = true
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	h, pStr, _ := net.SplitHostPort(ln.Addr().String())
	p, err := strconv.Atoi(pStr)
	if err != nil {
		t.Fatal(err)
	}
	return h, p
}

// --- Mock IMAP Client ---

type e2eMockClient struct {
	mu          sync.Mutex
	selectState imapworker.MailboxState
	fetchMsgs   []struct{ uid uint32; raw []byte }
	hasIdle     bool
	fetchCalled atomic.Int32
}

func (m *e2eMockClient) Login(_, _ string) error { return nil }

func (m *e2eMockClient) Select(_ string) (imapworker.MailboxState, error) {
	return m.selectState, nil
}

func (m *e2eMockClient) FetchSince(lastUID uint32, handler func(uid uint32, raw []byte) error) error {
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
	return nil
}

func (m *e2eMockClient) Idle(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (m *e2eMockClient) HasIdle() bool                       { return m.hasIdle }
func (m *e2eMockClient) Noop() error                         { return nil }
func (m *e2eMockClient) MoveUIDs(_ []uint32, _ string) error { return nil }
func (m *e2eMockClient) DeleteUIDs(_ []uint32) error         { return nil }
func (m *e2eMockClient) Close() error                        { return nil }

// --- Helpers ---

func e2eRepo(t *testing.T) *inbox.JSONRepo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	return inbox.NewJSONRepo(path, slog.Default())
}

func e2eLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// --- E2E Tests ---

func TestE2EOutboundHappyPath(t *testing.T) {
	backend := &e2eBackend{}
	host, port := startE2ESMTPServer(t, backend)
	logger := e2eLogger()

	sender := &transport.SMTPSender{
		Host: host, Port: port,
		Username: "testuser", Password: "testpass",
		From: "sender@test.com", TLSMode: "none",
		Logger: logger,
	}

	handler := pipe.NewHandler(sender, logger, "peer@test.com", "sender@test.com", nil, 0, 0)
	pkt := []byte("outbound-rns-packet")
	if err := handler.HandlePacket(context.Background(), pkt); err != nil {
		t.Fatal(err)
	}

	// Verify the SMTP server received the email.
	received := backend.Messages()
	if len(received) != 1 {
		t.Fatalf("smtp received %d messages, want 1", len(received))
	}
	if received[0].To != "peer@test.com" {
		t.Errorf("to = %q, want peer@test.com", received[0].To)
	}
	if !bytes.Contains(received[0].Data, []byte("RNS Transport Packet")) {
		t.Error("email missing RNS Transport Packet subject/body")
	}
}

func TestE2EInboundHappyPath(t *testing.T) {
	logger := e2eLogger()
	inboxRepo := e2eRepo(t)

	raw, _, err := envelope.Encode(envelope.Params{
		From:   "sender@test.com",
		To:     "receiver@test.com",
		Packet: []byte("inbound-rns-packet"),
	})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var injected [][]byte
	inject := func(_ context.Context, pkt []byte) error {
		mu.Lock()
		injected = append(injected, pkt)
		mu.Unlock()
		return nil
	}

	mock := &e2eMockClient{
		selectState: imapworker.MailboxState{UIDValidity: 1},
		fetchMsgs:   []struct{ uid uint32; raw []byte }{{uid: 1, raw: raw}},
	}

	w := imapworker.NewWorker(config.IMAPConfig{
		Folder:       "INBOX",
		PollInterval: config.Duration{Duration: time.Hour},
	}, inboxRepo, inject, logger)
	w.SetDial(func(ctx context.Context, onMailbox func()) (imapworker.Client, error) {
		return mock, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := len(injected)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("timed out waiting for injection")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(injected) != 1 || string(injected[0]) != "inbound-rns-packet" {
		t.Errorf("injected = %v, want [inbound-rns-packet]", injected)
	}
}

func TestE2EInboundInjectFailureRecovery(t *testing.T) {
	logger := e2eLogger()
	inboxRepo := e2eRepo(t)

	raw, _, err := envelope.Encode(envelope.Params{
		From:   "sender@test.com",
		To:     "receiver@test.com",
		Packet: []byte("retry-e2e-pkt"),
	})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var injected [][]byte
	callCount := 0
	inject := func(_ context.Context, pkt []byte) error {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		if callCount == 1 {
			return errors.New("transient")
		}
		injected = append(injected, pkt)
		return nil
	}

	w := imapworker.NewWorker(config.IMAPConfig{
		Folder:       "INBOX",
		PollInterval: config.Duration{Duration: time.Hour},
	}, inboxRepo, inject, logger)
	w.SetDial(func(ctx context.Context, onMailbox func()) (imapworker.Client, error) {
		return &e2eMockClient{
			selectState: imapworker.MailboxState{UIDValidity: 1},
			fetchMsgs:   []struct{ uid uint32; raw []byte }{{uid: 5, raw: raw}},
		}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	deadline := time.After(10 * time.Second)
	for {
		mu.Lock()
		n := len(injected)
		mu.Unlock()
		if n > 0 {
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

	mu.Lock()
	defer mu.Unlock()
	if string(injected[0]) != "retry-e2e-pkt" {
		t.Errorf("injected[0] = %q, want retry-e2e-pkt", injected[0])
	}

	cp, err := inboxRepo.GetCheckpoint(context.Background(), "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if cp != 5 {
		t.Errorf("checkpoint = %d, want 5", cp)
	}
}

func TestE2ECheckpointDurability(t *testing.T) {
	logger := e2eLogger()
	inboxRepo := e2eRepo(t)

	var msgs []struct{ uid uint32; raw []byte }
	for i := uint32(1); i <= 3; i++ {
		raw, _, err := envelope.Encode(envelope.Params{
			From:   "s@t.com",
			To:     "r@t.com",
			Packet: []byte(fmt.Sprintf("pkt-%d", i)),
		})
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, struct{ uid uint32; raw []byte }{uid: i, raw: raw})
	}

	inject := func(_ context.Context, _ []byte) error { return nil }

	sessionCount := 0
	w := imapworker.NewWorker(config.IMAPConfig{
		Folder:       "INBOX",
		PollInterval: config.Duration{Duration: time.Hour},
	}, inboxRepo, inject, logger)
	w.SetDial(func(ctx context.Context, onMailbox func()) (imapworker.Client, error) {
		sessionCount++
		return &e2eMockClient{
			selectState: imapworker.MailboxState{UIDValidity: 42},
			fetchMsgs:   msgs,
		}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Wait until checkpoint reaches 3 (all messages processed).
	deadline := time.After(3 * time.Second)
	for {
		cp, err := inboxRepo.GetCheckpoint(context.Background(), "INBOX", 42)
		if err != nil {
			t.Fatal(err)
		}
		if cp == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: checkpoint = %d, want 3", cp)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	cp, err := inboxRepo.GetCheckpoint(context.Background(), "INBOX", 42)
	if err != nil {
		t.Fatal(err)
	}
	if cp != 3 {
		t.Errorf("checkpoint = %d, want 3", cp)
	}

	// New worker should start from checkpoint 3.
	var dialCalled atomic.Bool
	w2 := imapworker.NewWorker(config.IMAPConfig{
		Folder:       "INBOX",
		PollInterval: config.Duration{Duration: time.Hour},
	}, inboxRepo, inject, logger)
	w2.SetDial(func(ctx context.Context, onMailbox func()) (imapworker.Client, error) {
		dialCalled.Store(true)
		return &e2eMockClient{
			selectState: imapworker.MailboxState{UIDValidity: 42},
			fetchMsgs:   msgs,
		}, nil
	})

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- w2.Run(ctx2) }()

	deadline2 := time.After(3 * time.Second)
	for {
		if dialCalled.Load() {
			break
		}
		select {
		case <-deadline2:
			cancel2()
			t.Fatal("timed out: second worker did not dial/fetch")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel2()
	<-done2

	if !dialCalled.Load() {
		t.Error("second worker did not dial/fetch")
	}
}

func TestE2ECheckpointNotAdvancedOnInjectFailure(t *testing.T) {
	// At-most-once guarantee: checkpoint must never advance when inject fails.
	logger := e2eLogger()
	inboxRepo := e2eRepo(t)

	raw, _, err := envelope.Encode(envelope.Params{
		From:   "sender@test.com",
		To:     "receiver@test.com",
		Packet: []byte("no-advance-pkt"),
	})
	if err != nil {
		t.Fatal(err)
	}

	inject := func(_ context.Context, _ []byte) error {
		return errors.New("inject permanently fails")
	}

	w := imapworker.NewWorker(config.IMAPConfig{
		Folder:       "INBOX",
		PollInterval: config.Duration{Duration: time.Hour},
	}, inboxRepo, inject, logger)
	w.SetDial(func(ctx context.Context, onMailbox func()) (imapworker.Client, error) {
		return &e2eMockClient{
			selectState: imapworker.MailboxState{UIDValidity: 1},
			fetchMsgs:   []struct{ uid uint32; raw []byte }{{uid: 5, raw: raw}},
		}, nil
	})

	// Run for a short window; inject always fails so checkpoint should stay at 0.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	cp, err := inboxRepo.GetCheckpoint(context.Background(), "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if cp != 0 {
		t.Errorf("checkpoint = %d, want 0 (must not advance when inject fails)", cp)
	}
}

func TestE2EBidirectionalFlow(t *testing.T) {
	// Full round-trip: encode via HandlePacket → real SMTP server → decode via IMAP worker.
	// Verifies that MIME encoding produced by Encode survives a real SMTP hop.
	backend := &e2eBackend{}
	host, port := startE2ESMTPServer(t, backend)
	logger := e2eLogger()

	sender := &transport.SMTPSender{
		Host: host, Port: port,
		Username: "testuser", Password: "testpass",
		From: "sender@test.com", TLSMode: "none",
		Logger: logger,
	}

	pkt := []byte("bidirectional-e2e-packet")
	handler := pipe.NewHandler(sender, logger, "peer@test.com", "sender@test.com", nil, 0, 0)
	if err := handler.HandlePacket(context.Background(), pkt); err != nil {
		t.Fatal(err)
	}

	received := backend.Messages()
	if len(received) != 1 {
		t.Fatalf("smtp received %d messages, want 1", len(received))
	}

	// Feed the raw email bytes received by SMTP into the IMAP worker.
	rawEmail := received[0].Data
	inboxRepo := e2eRepo(t)

	var mu sync.Mutex
	var injected [][]byte
	inject := func(_ context.Context, p []byte) error {
		mu.Lock()
		injected = append(injected, p)
		mu.Unlock()
		return nil
	}

	mock := &e2eMockClient{
		selectState: imapworker.MailboxState{UIDValidity: 1},
		fetchMsgs:   []struct{ uid uint32; raw []byte }{{uid: 1, raw: rawEmail}},
	}

	w := imapworker.NewWorker(config.IMAPConfig{
		Folder:       "INBOX",
		PollInterval: config.Duration{Duration: time.Hour},
	}, inboxRepo, inject, logger)
	w.SetDial(func(ctx context.Context, onMailbox func()) (imapworker.Client, error) {
		return mock, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(injected)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("timed out waiting for bidirectional injection")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(injected) != 1 || string(injected[0]) != string(pkt) {
		t.Errorf("injected = %q, want %q", injected, pkt)
	}
}

func TestE2EOutboundRetryRecovery(t *testing.T) {
	backend := &e2eBackend{}
	host, port := startE2ESMTPServer(t, backend)
	logger := e2eLogger()

	sender := &transport.SMTPSender{
		Host: host, Port: port,
		Username: "testuser", Password: "testpass",
		From: "sender@test.com", TLSMode: "none",
		Logger: logger,
	}

	// First send will fail, handler retries internally.
	backend.failNext.Store(true)
	handler := pipe.NewHandler(sender, logger, "peer@test.com", "sender@test.com", nil, 0, 0)
	if err := handler.HandlePacket(context.Background(), []byte("retry-outbound")); err != nil {
		t.Fatal(err)
	}

	// Verify email was eventually delivered after internal retry.
	received := backend.Messages()
	if len(received) != 1 {
		t.Fatalf("smtp received %d messages, want 1", len(received))
	}
}
