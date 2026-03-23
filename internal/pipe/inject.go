package pipe

import (
	"context"
	"errors"
	"time"

	rnspipe "github.com/x3ps/go-rns-pipe"
)

// InjectWithRetry calls receive(pkt) in a loop, retrying on ErrOffline and
// ErrNotStarted with a 1-second backoff. It returns immediately on success,
// non-retryable errors, or context cancellation.
func InjectWithRetry(ctx context.Context, pkt []byte, receive func([]byte) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := receive(pkt)
		if err == nil {
			return nil
		}
		if !errors.Is(err, rnspipe.ErrOffline) && !errors.Is(err, rnspipe.ErrNotStarted) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
