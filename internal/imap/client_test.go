package imap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/x3ps/rns-iface-email/internal/envelope"
)

func TestHasUIDPlusWithExplicitCap(t *testing.T) {
	caps := goimap.CapSet{}
	caps[goimap.CapUIDPlus] = struct{}{}

	if !hasUIDPlus(caps) {
		t.Error("hasUIDPlus should return true when UIDPLUS is explicitly present")
	}
}

func TestHasIdleWithExplicitCap(t *testing.T) {
	caps := goimap.CapSet{}
	caps[goimap.CapIdle] = struct{}{}

	if !hasIdle(caps) {
		t.Error("hasIdle should return true when IDLE is explicitly present")
	}
}

func TestHasUIDPlusWithIMAP4rev2(t *testing.T) {
	// IMAP4rev2 implies UIDPLUS. CapSet.Has() should handle this.
	caps := goimap.CapSet{}
	caps[goimap.CapIMAP4rev2] = struct{}{}

	if !hasUIDPlus(caps) {
		t.Error("hasUIDPlus should return true when IMAP4rev2 is present (implied capability)")
	}
}

func TestHasIdleWithIMAP4rev2(t *testing.T) {
	// IMAP4rev2 implies IDLE. CapSet.Has() should handle this.
	caps := goimap.CapSet{}
	caps[goimap.CapIMAP4rev2] = struct{}{}

	if !hasIdle(caps) {
		t.Error("hasIdle should return true when IMAP4rev2 is present (implied capability)")
	}
}

func TestHasUIDPlusEmptyCapSet(t *testing.T) {
	caps := goimap.CapSet{}

	if hasUIDPlus(caps) {
		t.Error("hasUIDPlus should return false for empty CapSet")
	}
}

func TestHasIdleEmptyCapSet(t *testing.T) {
	caps := goimap.CapSet{}

	if hasIdle(caps) {
		t.Error("hasIdle should return false for empty CapSet")
	}
}

func TestHasMove(t *testing.T) {
	// Explicit MOVE capability.
	caps := goimap.CapSet{}
	caps[goimap.CapMove] = struct{}{}
	if !hasMove(caps) {
		t.Error("hasMove should return true when MOVE is explicitly present")
	}

	// IMAP4rev2 implies MOVE.
	caps2 := goimap.CapSet{}
	caps2[goimap.CapIMAP4rev2] = struct{}{}
	if !hasMove(caps2) {
		t.Error("hasMove should return true when IMAP4rev2 is present (implied capability)")
	}

	// Empty → false.
	if hasMove(goimap.CapSet{}) {
		t.Error("hasMove should return false for empty CapSet")
	}
}

func TestRequireSafeMove(t *testing.T) {
	tests := []struct {
		name    string
		caps    goimap.CapSet
		wantErr bool
	}{
		{
			name:    "MOVE only",
			caps:    func() goimap.CapSet { c := goimap.CapSet{}; c[goimap.CapMove] = struct{}{}; return c }(),
			wantErr: false,
		},
		{
			name:    "UIDPLUS only",
			caps:    func() goimap.CapSet { c := goimap.CapSet{}; c[goimap.CapUIDPlus] = struct{}{}; return c }(),
			wantErr: false,
		},
		{
			name: "both MOVE and UIDPLUS",
			caps: func() goimap.CapSet {
				c := goimap.CapSet{}
				c[goimap.CapMove] = struct{}{}
				c[goimap.CapUIDPlus] = struct{}{}
				return c
			}(),
			wantErr: false,
		},
		{
			name:    "neither",
			caps:    goimap.CapSet{},
			wantErr: true,
		},
		{
			name:    "IMAP4rev2 implies both",
			caps:    func() goimap.CapSet { c := goimap.CapSet{}; c[goimap.CapIMAP4rev2] = struct{}{}; return c }(),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireSafeMove(tt.caps)
			if tt.wantErr && !errors.Is(err, errUnsafeMove) {
				t.Errorf("expected errUnsafeMove, got: %v", err)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestRequireUIDPlusGate(t *testing.T) {
	// No capabilities → errNoUIDPlus
	caps := goimap.CapSet{}
	if err := requireUIDPlus(caps); !errors.Is(err, errNoUIDPlus) {
		t.Errorf("expected errNoUIDPlus for empty caps, got: %v", err)
	}

	// Explicit UIDPLUS → nil
	caps[goimap.CapUIDPlus] = struct{}{}
	if err := requireUIDPlus(caps); err != nil {
		t.Errorf("unexpected error with UIDPLUS: %v", err)
	}

	// IMAP4rev2 implies UIDPLUS → nil
	caps2 := goimap.CapSet{}
	caps2[goimap.CapIMAP4rev2] = struct{}{}
	if err := requireUIDPlus(caps2); err != nil {
		t.Errorf("unexpected error with IMAP4rev2: %v", err)
	}
}

func TestIdleWaitServerDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waitCh := make(chan error, 1)
	waitErr := errors.New("server BYE")
	waitCh <- waitErr

	closeCalled := false
	closeFn := func() error {
		closeCalled = true
		return nil
	}

	err := idleWait(ctx, waitCh, closeFn)
	if !errors.Is(err, waitErr) {
		t.Errorf("got %v, want %v", err, waitErr)
	}
	if closeCalled {
		t.Error("closeFn should not be called when waitCh fires first")
	}
}

func TestIdleWaitContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	waitCh := make(chan error, 1)
	closeFn := func() error {
		return nil
	}

	// Cancel context, then provide waitCh result.
	cancel()
	go func() {
		time.Sleep(10 * time.Millisecond)
		waitCh <- nil
	}()

	err := idleWait(ctx, waitCh, closeFn)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestIdleWaitContextDoneCloseError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	waitCh := make(chan error, 1)
	closeErr := errors.New("close failed")
	closeFn := func() error {
		return closeErr
	}

	cancel()
	go func() {
		time.Sleep(10 * time.Millisecond)
		waitCh <- nil
	}()

	err := idleWait(ctx, waitCh, closeFn)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, closeErr) {
		t.Errorf("expected wrapped close error, got: %v", err)
	}
}

func TestReadLiteralWithinLimit(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	buf, oversized, err := readLiteral(bytes.NewReader(data), 200)
	if err != nil {
		t.Fatal(err)
	}
	if oversized {
		t.Error("expected oversized=false")
	}
	if len(buf) != 100 {
		t.Errorf("len(buf) = %d, want 100", len(buf))
	}
}

func TestReadLiteralExceedsLimit(t *testing.T) {
	limit := 1 << 20 // 1 MiB
	data := bytes.Repeat([]byte("x"), 10*1024*1024) // 10 MB
	r := &countingReader{r: bytes.NewReader(data)}
	buf, oversized, err := readLiteral(r, limit)
	if err != nil {
		t.Fatal(err)
	}
	if !oversized {
		t.Error("expected oversized=true")
	}
	if len(buf) != limit {
		t.Errorf("len(buf) = %d, want %d", len(buf), limit)
	}
	// Source should be fully drained.
	if r.n != int64(len(data)) {
		t.Errorf("source bytes consumed = %d, want %d (fully drained)", r.n, len(data))
	}
}

func TestReadLiteralDrainsExcess(t *testing.T) {
	data := bytes.Repeat([]byte("A"), 5000)
	r := &countingReader{r: bytes.NewReader(data)}
	_, _, err := readLiteral(r, 100)
	if err != nil {
		t.Fatal(err)
	}
	if r.n != int64(len(data)) {
		t.Errorf("total bytes consumed = %d, want %d", r.n, len(data))
	}
}

// countingReader wraps an io.Reader and counts total bytes read.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func TestMaxFetchLiteralSizeCoversMaxPacket(t *testing.T) {
	pkt := bytes.Repeat([]byte("P"), envelope.MaxPacketSize)
	raw, _, err := envelope.Encode(envelope.Params{
		From:   "a@b.com",
		To:     "c@d.com",
		Packet: pkt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxFetchLiteralSize {
		t.Errorf("Encode() produced %d bytes, exceeds maxFetchLiteralSize=%d", len(raw), maxFetchLiteralSize)
	}
}
