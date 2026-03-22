package imap

import (
	"errors"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
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
