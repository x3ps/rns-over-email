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
