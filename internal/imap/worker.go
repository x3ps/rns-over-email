package imap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/x3ps/rns-iface-email/internal/config"
	"github.com/x3ps/rns-iface-email/internal/envelope"
	"github.com/x3ps/rns-iface-email/internal/inbox"
)

const (
	idleRestart   = 25 * time.Minute
	maxIdleErrors = 3
)

var errDial = errors.New("dial failed")

// Worker polls or idles on an IMAP mailbox and injects inbound packets.
type Worker struct {
	cfg        config.IMAPConfig
	peerEmail  string // normalized bare address of the remote peer
	localEmail string // normalized bare address of the local endpoint
	repo       inbox.Repository
	inject     func(context.Context, []byte) error
	dial       func(ctx context.Context, onMailbox func()) (Client, error)
	logger     *slog.Logger
	setOnline  func(bool)
}

// NewWorker creates an IMAP worker. The inject function should call iface.Receive.
// peerEmail and localEmail are bare email addresses used to validate inbound
// transport envelopes (point-to-point contract).
func NewWorker(cfg config.IMAPConfig, peerEmail, localEmail string, repo inbox.Repository, inject func(context.Context, []byte) error, logger *slog.Logger) *Worker {
	w := &Worker{
		cfg:        cfg,
		peerEmail:  peerEmail,
		localEmail: localEmail,
		repo:       repo,
		inject:     inject,
		logger:     logger.With("component", "imap-worker"),
	}
	w.dial = func(ctx context.Context, onMailbox func()) (Client, error) {
		return Dial(ctx, cfg.Host, cfg.Port, cfg.TLS, onMailbox)
	}
	return w
}

// SetDial replaces the default dial function (for testing).
func (w *Worker) SetDial(fn func(ctx context.Context, onMailbox func()) (Client, error)) {
	w.dial = fn
}

// SetOnline registers a callback that is called with true when an IMAP session
// is fully established (login + select + catch-up done) and with false when the
// session ends for any reason. If not set, no online/offline signalling occurs.
func (w *Worker) SetOnline(fn func(bool)) { w.setOnline = fn }

func (w *Worker) callSetOnline(online bool) {
	if w.setOnline != nil {
		w.setOnline(online)
	}
}

// Run is the main reconnect loop. It blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	base := time.Duration(w.cfg.ReconnectDelay) * time.Second
	maxBackoff := time.Duration(w.cfg.MaxReconnectDelay) * time.Second
	backoff := base
	for {
		err := w.runSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		w.logger.Warn("imap session ended", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		// Reset or grow backoff AFTER sleeping so the current sleep uses the
		// value that was relevant when the error occurred.
		if isDial(err) {
			backoff = min(backoff*2, maxBackoff)
		} else {
			backoff = base
		}
	}
}

// isDial reports whether the error came from the dial phase.
func isDial(err error) bool {
	return errors.Is(err, errDial)
}

func (w *Worker) runSession(ctx context.Context) error {
	defer w.callSetOnline(false)

	existsCh := make(chan struct{}, 1)

	client, err := w.dial(ctx, func() {
		select {
		case existsCh <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return fmt.Errorf("%w: %w", errDial, err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Login(w.cfg.Username, w.cfg.Password); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	mbox, err := client.Select(w.cfg.Folder)
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}

	lastUID, err := w.repo.GetCheckpoint(ctx, w.cfg.Folder, mbox.UIDValidity)
	if err != nil {
		return fmt.Errorf("get checkpoint: %w", err)
	}

	// Catch-up fetch.
	newLastUID, err := w.fetchAndProcess(ctx, client, lastUID, w.cfg.Folder, mbox.UIDValidity)
	if err != nil {
		return fmt.Errorf("catch-up fetch: %w", err)
	}
	lastUID = newLastUID

	// Session is fully established — both directions are operational.
	w.callSetOnline(true)

	if client.HasIdle() {
		return w.idleLoop(ctx, client, lastUID, w.cfg.Folder, mbox.UIDValidity, existsCh)
	}
	return w.pollLoop(ctx, client, lastUID, w.cfg.Folder, mbox.UIDValidity)
}

// canonicalAddr returns a comparison-ready email address:
// display name stripped, domain lowercased, local-part unchanged.
// Per RFC 5321 §2.3.5 domain names are case-insensitive.
func canonicalAddr(addr string) string {
	bare := addr
	if parsed, err := mail.ParseAddress(addr); err == nil {
		bare = parsed.Address
	}
	at := strings.LastIndex(bare, "@")
	if at < 0 {
		return bare
	}
	return bare[:at+1] + strings.ToLower(bare[at+1:])
}

func (w *Worker) fetchAndProcess(ctx context.Context, client Client, lastUID uint32, folder string, uidValidity uint32) (uint32, error) {
	var processedUIDs []uint32
	var skippedUIDs []uint32
	var failedUID uint32
	var decodeFailedUID uint32

	err := client.FetchSince(lastUID, func(uid uint32, raw []byte) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		decoded, decErr := envelope.Decode(raw)
		if errors.Is(decErr, envelope.ErrNotTransport) {
			w.logger.Debug("skipping non-transport message", "uid", uid)
			skippedUIDs = append(skippedUIDs, uid)
			return nil
		}
		if decErr != nil {
			w.logger.Warn("decode failed, preserving for retry", "uid", uid, "error", decErr)
			if decodeFailedUID == 0 || uid < decodeFailedUID {
				decodeFailedUID = uid
			}
			return nil
		}

		// Point-to-point contract: verify sender and recipient.
		// Use canonicalAddr for comparison (domain-case-insensitive per RFC 5321).
		if w.peerEmail != "" && canonicalAddr(decoded.From) != canonicalAddr(w.peerEmail) {
			w.logger.Debug("skipping message from non-peer sender",
				"uid", uid, "from", decoded.From, "expected", w.peerEmail)
			skippedUIDs = append(skippedUIDs, uid)
			return nil
		}
		if w.localEmail != "" && canonicalAddr(decoded.To) != canonicalAddr(w.localEmail) {
			w.logger.Debug("skipping message to wrong recipient",
				"uid", uid, "to", decoded.To, "expected", w.localEmail)
			skippedUIDs = append(skippedUIDs, uid)
			return nil
		}

		if err := w.inject(ctx, decoded.Packet); err != nil {
			failedUID = uid
			return fmt.Errorf("inject uid %d: %w", uid, err)
		}

		w.logger.Info("packet received",
			"uid", uid, "message_id", decoded.MessageID,
			"size", len(decoded.Packet))
		processedUIDs = append(processedUIDs, uid)
		return nil
	})

	// Compute safe checkpoint: advance only up to (but not past) any failure
	// (inject or decode). This prevents gaps when the server returns UIDs out
	// of order (RFC 3501 does not guarantee FETCH response ordering).
	//
	// ceilingUID is the lowest UID that failed — we must not advance past it.
	var ceilingUID uint32
	if failedUID > 0 && decodeFailedUID > 0 {
		ceilingUID = min(failedUID, decodeFailedUID)
	} else if failedUID > 0 {
		ceilingUID = failedUID
	} else {
		ceilingUID = decodeFailedUID
	}

	// Merge processed and skipped UIDs into a single sorted list of
	// "advanceable" UIDs. Skipped (non-transport / wrong sender) UIDs
	// are safe to advance past — they will never become processable.
	advanceable := make([]uint32, 0, len(processedUIDs)+len(skippedUIDs))
	advanceable = append(advanceable, processedUIDs...)
	advanceable = append(advanceable, skippedUIDs...)

	safeUID := lastUID
	if len(advanceable) > 0 {
		sort.Slice(advanceable, func(i, j int) bool {
			return advanceable[i] < advanceable[j]
		})
		for _, uid := range advanceable {
			if uid <= safeUID {
				continue
			}
			if ceilingUID > 0 && uid > ceilingUID {
				break
			}
			safeUID = uid
		}
		if safeUID > lastUID {
			if cpErr := w.repo.AdvanceCheckpoint(ctx, folder, uidValidity, safeUID); cpErr != nil {
				w.logger.Warn("advance checkpoint failed", "error", cpErr)
			}
		}
	}

	// Detect stuck checkpoint: decode-failed UID with no progress.
	if decodeFailedUID > 0 && safeUID == lastUID {
		w.logger.Error("checkpoint stuck: message permanently undecodable — delete it manually",
			"folder", folder, "stuck_uid", decodeFailedUID)
	}

	// Best-effort cleanup of processed messages. Only clean up UIDs at or
	// below the safe checkpoint to avoid deleting messages above a gap that
	// would be unrecoverable after a crash.
	// Skipped UIDs are never cleaned up — they remain in the mailbox for the user.
	if err == nil && safeUID > lastUID {
		var safeUIDs []uint32
		for _, uid := range processedUIDs {
			if uid <= safeUID {
				safeUIDs = append(safeUIDs, uid)
			}
		}
		if len(safeUIDs) > 0 {
			w.cleanup(client, safeUIDs)
		}
	}

	return safeUID, err
}

// cleanup removes or moves already-processed messages on a best-effort basis.
// Errors are logged and swallowed intentionally: the checkpoint has already
// been advanced, so the messages will not be re-processed even if cleanup
// fails.  Leaving them undeleted/unmoved is safe — the only consequence is
// that the mailbox accumulates stale messages until the next successful run.
// No retry is performed here; transient errors will resolve on the next
// session reconnect.
func (w *Worker) cleanup(client Client, uids []uint32) {
	switch w.cfg.Cleanup.Mode {
	case "delete":
		if err := client.DeleteUIDs(uids); err != nil {
			if errors.Is(err, errNoUIDPlus) {
				w.logger.Warn("delete cleanup skipped: server lacks UIDPLUS; messages will remain in mailbox")
			} else {
				w.logger.Warn("cleanup delete failed", "uids", uids, "error", err)
			}
		}
	case "move":
		if err := client.MoveUIDs(uids, w.cfg.Cleanup.TargetFolder); err != nil {
			w.logger.Warn("cleanup move failed", "uids", uids, "dest", w.cfg.Cleanup.TargetFolder, "error", err)
		}
	}
}

func (w *Worker) idleLoop(ctx context.Context, client Client, lastUID uint32, folder string, uidValidity uint32, existsCh <-chan struct{}) error {
	idleErrors := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		idleCtx, idleCancel := context.WithTimeout(ctx, idleRestart)
		idleDone := make(chan error, 1)
		go func() {
			idleDone <- client.Idle(idleCtx)
		}()

		// Wait for EXISTS notification, timeout, or context cancellation.
		select {
		case <-existsCh:
			idleCancel()
			<-idleDone
		case err := <-idleDone:
			idleCancel()
			if err != nil && ctx.Err() == nil {
				idleErrors++
				w.logger.Warn("idle error", "error", err, "consecutive", idleErrors)
				if idleErrors >= maxIdleErrors {
					w.logger.Warn("too many idle errors, falling back to poll")
					return w.pollLoop(ctx, client, lastUID, folder, uidValidity)
				}
				continue
			}
		case <-ctx.Done():
			idleCancel()
			<-idleDone
			return nil
		}

		idleErrors = 0
		newLastUID, err := w.fetchAndProcess(ctx, client, lastUID, folder, uidValidity)
		if err != nil {
			return fmt.Errorf("fetch after idle: %w", err)
		}
		lastUID = newLastUID
	}
}

func (w *Worker) pollLoop(ctx context.Context, client Client, lastUID uint32, folder string, uidValidity uint32) error {
	interval := w.cfg.PollInterval.Duration
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := client.Noop(); err != nil {
				return fmt.Errorf("noop: %w", err)
			}
			newLastUID, err := w.fetchAndProcess(ctx, client, lastUID, folder, uidValidity)
			if err != nil {
				return fmt.Errorf("fetch after poll: %w", err)
			}
			lastUID = newLastUID
		}
	}
}
