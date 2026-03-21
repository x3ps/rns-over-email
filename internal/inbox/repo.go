package inbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Repository defines the inbound message storage operations.
type Repository interface {
	AdvanceCheckpoint(ctx context.Context, folder string, uidValidity uint32, uid uint32) error
	GetCheckpoint(ctx context.Context, folder string, uidValidity uint32) (lastUID uint32, err error)
}

type checkpoint struct {
	Folder      string `json:"folder"`
	UIDValidity uint32 `json:"uidvalidity"`
	LastUID     uint32 `json:"last_uid"`
}

// JSONRepo implements Repository using an atomic JSON file.
type JSONRepo struct {
	path   string
	mu     sync.Mutex
	logger *slog.Logger
}

// NewJSONRepo creates a new JSON-file-backed inbox repository.
func NewJSONRepo(path string, logger *slog.Logger) *JSONRepo {
	return &JSONRepo{path: path, logger: logger}
}

func (r *JSONRepo) load() (*checkpoint, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &checkpoint{}, nil
		}
		return nil, err
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		r.logger.Warn("checkpoint file corrupt, starting from UID 0", "path", r.path, "error", err)
		return &checkpoint{}, nil
	}
	return &cp, nil
}

func (r *JSONRepo) save(cp *checkpoint) error {
	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func (r *JSONRepo) GetCheckpoint(_ context.Context, folder string, uidValidity uint32) (uint32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cp, err := r.load()
	if err != nil {
		return 0, err
	}
	if cp.Folder != folder || cp.UIDValidity != uidValidity {
		return 0, nil
	}
	return cp.LastUID, nil
}

func (r *JSONRepo) AdvanceCheckpoint(_ context.Context, folder string, uidValidity uint32, uid uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	cp, err := r.load()
	if err != nil {
		return err
	}

	if cp.Folder != folder || cp.UIDValidity != uidValidity {
		// UIDVALIDITY changed — reset.
		cp = &checkpoint{
			Folder:      folder,
			UIDValidity: uidValidity,
			LastUID:     uid,
		}
	} else if uid > cp.LastUID {
		cp.LastUID = uid
	}

	return r.save(cp)
}
