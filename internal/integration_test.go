package internal

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/x3ps/rns-iface-email/internal/envelope"
	"github.com/x3ps/rns-iface-email/internal/inbox"
	"github.com/x3ps/rns-iface-email/internal/pipe"
)

// mockSender records send calls.
type mockSender struct {
	mu       sync.Mutex
	calls    []sendCall
	failNext int
}

type sendCall struct {
	to  string
	msg []byte
}

func (s *mockSender) Send(_ context.Context, to string, msg []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext > 0 {
		s.failNext--
		return errors.New("transient SMTP error")
	}
	s.calls = append(s.calls, sendCall{to: to, msg: msg})
	return nil
}

func (s *mockSender) Probe(_ context.Context) error { return nil }

func TestFullPipeline(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sender := &mockSender{}
	handler := pipe.NewHandler(sender, logger, "p1@test.com", "transport@test.com", nil, 0, 0)
	packet := []byte("full-pipeline-test-data")
	if err := handler.HandlePacket(context.Background(), packet); err != nil {
		t.Fatal(err)
	}

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}
	if sender.calls[0].to != "p1@test.com" {
		t.Errorf("to = %q, want p1@test.com", sender.calls[0].to)
	}

	// Verify decoded envelope matches original packet.
	decoded, err := envelope.Decode(sender.calls[0].msg)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded.Packet) != string(packet) {
		t.Errorf("decoded packet = %q, want %q", decoded.Packet, packet)
	}
}

func TestRetryPipeline(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sender := &mockSender{failNext: 2}
	handler := pipe.NewHandler(sender, logger, "p1@test.com", "transport@test.com", nil, 0, 0)
	if err := handler.HandlePacket(context.Background(), []byte("retry-test")); err != nil {
		t.Fatal(err)
	}

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send after retries, got %d", len(sender.calls))
	}
}

func TestInboundRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	inboxRepo := inbox.NewJSONRepo(path, slog.Default())
	ctx := context.Background()

	// Encode an email as if it arrived via IMAP.
	packet := []byte("inbound-roundtrip-data")
	raw, _, err := envelope.Encode(envelope.Params{
		From:   "remote@test.com",
		To:     "local@test.com",
		Packet: packet,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Decode like the IMAP worker would.
	decoded, err := envelope.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded.Packet) != string(packet) {
		t.Errorf("packet = %q, want %q", decoded.Packet, packet)
	}

	// Advance checkpoint.
	if err := inboxRepo.AdvanceCheckpoint(ctx, "INBOX", 42, 7); err != nil {
		t.Fatal(err)
	}

	// Verify checkpoint.
	uid, err := inboxRepo.GetCheckpoint(ctx, "INBOX", 42)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 7 {
		t.Errorf("checkpoint = %d, want 7", uid)
	}
}

func TestFullPipelinePacketNearMTUBoundary(t *testing.T) {
	// Verify a 500-byte (default MTU) packet passes through encode→send→decode without truncation.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sender := &mockSender{}
	handler := pipe.NewHandler(sender, logger, "p1@test.com", "transport@test.com", nil, 0, 0)

	packet := make([]byte, 500)
	for i := range packet {
		packet[i] = byte(i % 256)
	}

	if err := handler.HandlePacket(context.Background(), packet); err != nil {
		t.Fatal(err)
	}

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}

	decoded, err := envelope.Decode(sender.calls[0].msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Packet) != len(packet) {
		t.Errorf("decoded packet length = %d, want %d (no truncation at MTU boundary)", len(decoded.Packet), len(packet))
	}
	for i, b := range decoded.Packet {
		if b != packet[i] {
			t.Errorf("byte mismatch at index %d: got %02x, want %02x", i, b, packet[i])
			break
		}
	}
}

func TestCheckpointPersistenceAcrossSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	ctx := context.Background()

	// Session 1: write checkpoint.
	repo1 := inbox.NewJSONRepo(path, slog.Default())
	if err := repo1.AdvanceCheckpoint(ctx, "INBOX", 100, 42); err != nil {
		t.Fatal(err)
	}

	// Session 2: read it back via new repo instance.
	repo2 := inbox.NewJSONRepo(path, slog.Default())
	uid, err := repo2.GetCheckpoint(ctx, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 42 {
		t.Errorf("checkpoint = %d, want 42", uid)
	}
}
