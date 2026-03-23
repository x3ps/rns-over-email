package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newTestRepo(t *testing.T) *JSONRepo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	return NewJSONRepo(path, slog.Default())
}

func TestGetCheckpointUnknown(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	uid, err := repo.GetCheckpoint(ctx, "INBOX", 12345)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 0 {
		t.Errorf("expected 0 for unknown folder/uidvalidity, got %d", uid)
	}
}

func TestAdvanceCheckpoint(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 10); err != nil {
		t.Fatal(err)
	}

	uid, err := repo.GetCheckpoint(ctx, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 10 {
		t.Errorf("checkpoint = %d, want 10", uid)
	}

	// Should not regress.
	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 5); err != nil {
		t.Fatal(err)
	}
	uid, err = repo.GetCheckpoint(ctx, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 10 {
		t.Errorf("checkpoint regressed: got %d, want 10", uid)
	}

	// Should advance further.
	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 20); err != nil {
		t.Fatal(err)
	}
	uid, err = repo.GetCheckpoint(ctx, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 20 {
		t.Errorf("checkpoint = %d, want 20", uid)
	}
}

func TestCheckpointDoesNotRegress(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 20); err != nil {
		t.Fatal(err)
	}
	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 10); err != nil {
		t.Fatal(err)
	}

	uid, err := repo.GetCheckpoint(ctx, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 20 {
		t.Errorf("checkpoint regressed: got %d, want 20", uid)
	}
}

func TestUIDValidityReset(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 50); err != nil {
		t.Fatal(err)
	}

	// New uidvalidity 200 — should start from 0.
	uid, err := repo.GetCheckpoint(ctx, "INBOX", 200)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 0 {
		t.Errorf("new uidvalidity should start from 0, got %d", uid)
	}

	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 200, 3); err != nil {
		t.Fatal(err)
	}

	uid, err = repo.GetCheckpoint(ctx, "INBOX", 200)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 3 {
		t.Errorf("checkpoint = %d, want 3", uid)
	}
}

func TestAdvanceCheckpointIdempotent(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 10); err != nil {
		t.Fatal(err)
	}
	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 10); err != nil {
		t.Fatalf("idempotent advance failed: %v", err)
	}
}

func TestCheckpointPersistenceAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	ctx := context.Background()

	// First instance writes checkpoint.
	repo1 := NewJSONRepo(path, slog.Default())
	if err := repo1.AdvanceCheckpoint(ctx, "INBOX", 100, 5); err != nil {
		t.Fatal(err)
	}

	// Second instance reads it back.
	repo2 := NewJSONRepo(path, slog.Default())
	uid, err := repo2.GetCheckpoint(ctx, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 5 {
		t.Errorf("checkpoint = %d, want 5", uid)
	}
}

func TestCorruptCheckpointFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")

	// Write garbage to the checkpoint file.
	if err := os.WriteFile(path, []byte("{{{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	repo := NewJSONRepo(path, logger)
	ctx := context.Background()

	uid, err := repo.GetCheckpoint(ctx, "INBOX", 100)
	if err != nil {
		t.Fatalf("expected graceful recovery, got error: %v", err)
	}
	if uid != 0 {
		t.Errorf("expected UID 0 after corrupt file, got %d", uid)
	}

	if !bytes.Contains(logBuf.Bytes(), []byte("checkpoint file corrupt")) {
		t.Error("expected 'checkpoint file corrupt' warning in logs")
	}
}

func TestSecondFolderOverwritesFirst(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	// Advance INBOX.
	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 10); err != nil {
		t.Fatal(err)
	}

	// Advance Archive — overwrites the single-folder store.
	if err := repo.AdvanceCheckpoint(ctx, "Archive", 200, 20); err != nil {
		t.Fatal(err)
	}

	// Archive should return its checkpoint.
	uid, err := repo.GetCheckpoint(ctx, "Archive", 200)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 20 {
		t.Errorf("Archive checkpoint = %d, want 20", uid)
	}

	// INBOX with old validity should return 0 (store was overwritten by Archive).
	uid, err = repo.GetCheckpoint(ctx, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 0 {
		t.Errorf("INBOX checkpoint = %d, want 0 (overwritten by Archive)", uid)
	}
}

func TestAdvanceCheckpointConcurrent(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		uid := uint32(i + 1)
		go func() {
			defer wg.Done()
			if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, uid); err != nil {
				t.Errorf("AdvanceCheckpoint(%d): %v", uid, err)
			}
		}()
	}
	wg.Wait()

	// All writes are serialized by the mutex; final checkpoint should be max UID.
	finalUID, err := repo.GetCheckpoint(ctx, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if finalUID != N {
		t.Errorf("concurrent checkpoint = %d, want %d", finalUID, N)
	}
}

func TestAtomicWriteVerification(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	repo := NewJSONRepo(path, slog.Default())
	ctx := context.Background()

	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 42); err != nil {
		t.Fatal(err)
	}

	// The checkpoint file should be well-formed JSON.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("checkpoint file not found: %v", err)
	}
	var cp map[string]interface{}
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Errorf("checkpoint file is not valid JSON: %v", err)
	}

	// No .tmp file should remain after a successful write.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("leftover .tmp file found after successful AdvanceCheckpoint")
	}
}

func TestCheckpointDirectoryCreation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	path := filepath.Join(dir, "checkpoint.json")
	repo := NewJSONRepo(path, slog.Default())
	ctx := context.Background()

	if err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 5); err != nil {
		t.Fatalf("expected MkdirAll to create nested dirs, got: %v", err)
	}

	uid, err := repo.GetCheckpoint(ctx, "INBOX", 100)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 5 {
		t.Errorf("checkpoint = %d, want 5", uid)
	}
}

func TestSave_MkdirAllError(t *testing.T) {
	// Place a regular file where the parent directory should be.
	// MkdirAll will fail because it can't create a directory over a file.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("I am a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	// checkpoint path: blocker/sub/checkpoint.json → MkdirAll("blocker/sub") fails.
	path := filepath.Join(blocker, "sub", "checkpoint.json")
	repo := NewJSONRepo(path, slog.Default())
	ctx := context.Background()

	err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 5)
	if err == nil {
		t.Fatal("expected MkdirAll error, got nil")
	}
}

func TestSave_RenameConflict(t *testing.T) {
	// Target path is a directory, so os.Rename(tmp, path) should fail.
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}

	repo := NewJSONRepo(path, slog.Default())
	ctx := context.Background()

	err := repo.AdvanceCheckpoint(ctx, "INBOX", 100, 5)
	if err == nil {
		t.Fatal("expected Rename error when target is a directory, got nil")
	}
}

func TestLoad_NulBytePath(t *testing.T) {
	// Path containing NUL byte → ReadFile error (not IsNotExist).
	path := filepath.Join(t.TempDir(), "check\x00point.json")
	repo := NewJSONRepo(path, slog.Default())
	ctx := context.Background()

	_, err := repo.GetCheckpoint(ctx, "INBOX", 100)
	if err == nil {
		t.Fatal("expected error from NUL byte path, got nil")
	}
	// Should not be treated as "not exist" (which returns uid=0, err=nil).
	// The error should propagate.
}
