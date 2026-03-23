package pipe

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/x3ps/rns-iface-email/internal/envelope"
	"github.com/x3ps/rns-iface-email/internal/transport"
)

const (
	maxRetries  = 3
	sendTimeout = 30 * time.Second
)

// Handler processes packets received from the RNS pipe.
type Handler struct {
	sender           transport.Sender
	logger           *slog.Logger
	email            string
	smtpFrom         string
	setOnline        func(bool)
	recoveryDelay    time.Duration
	maxRecoveryDelay time.Duration
	recovering       atomic.Bool
}

// NewHandler creates a Handler. setOnline and recoveryDelay are optional: if
// setOnline is nil or recoveryDelay is zero, recovery mode is disabled and the
// handler behaves as before (best-effort, no online/offline signalling).
func NewHandler(sender transport.Sender, logger *slog.Logger, email, smtpFrom string, setOnline func(bool), recoveryDelay, maxRecoveryDelay time.Duration) *Handler {
	return &Handler{
		sender:           sender,
		logger:           logger,
		email:            email,
		smtpFrom:         smtpFrom,
		setOnline:        setOnline,
		recoveryDelay:    recoveryDelay,
		maxRecoveryDelay: maxRecoveryDelay,
	}
}

// HandlePacket is the OnSend callback. It encodes the packet as a MIME envelope
// and sends it via SMTP with a small retry budget. If all retries fail the
// packet is lost at this layer (best-effort, at-most-once delivery) and a
// probe-based recovery goroutine is started to bring the interface back online.
func (h *Handler) HandlePacket(ctx context.Context, pkt []byte) error {
	envBytes, messageID, err := envelope.Encode(envelope.Params{
		From:   h.smtpFrom,
		To:     h.email,
		Packet: pkt,
	})
	if err != nil {
		h.logger.Error("envelope encode failed", "error", err, "message_id", messageID)
		return nil
	}

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	var lastErr error
	backoff := time.Second
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := h.sender.Send(sendCtx, h.email, envBytes); err != nil {
			var unknown *transport.ErrDataOutcomeUnknown
			if errors.As(err, &unknown) {
				h.logger.Error("packet delivery ambiguous: DATA sent but response unclear; not retrying",
					"message_id", messageID, "error", err)
				h.beginRecovery(ctx)
				return nil
			}
			lastErr = err
			h.logger.Warn("send failed",
				"message_id", messageID,
				"attempt", attempt, "max_attempts", maxRetries, "error", err)
			if attempt < maxRetries {
				select {
				case <-sendCtx.Done():
					h.logger.Error("packet lost: send timeout",
						"message_id", messageID,
						"delivery", "lost", "error", sendCtx.Err())
					return nil
				case <-time.After(backoff):
				}
				backoff *= 2
			}
			continue
		}

		h.logger.Info("packet sent",
			"message_id", messageID,
			"email", h.email, "size", len(pkt))
		return nil
	}

	h.logger.Error("packet lost: all retries exhausted",
		"message_id", messageID,
		"delivery", "lost", "error", lastErr)

	h.beginRecovery(ctx)

	return nil
}

// beginRecovery starts a recovery goroutine if one is not already running.
// Uses CompareAndSwap to ensure at most one recovery goroutine is active.
// Called by both HandlePacket (on exhausted retries) and StartupProbe (on
// initial failure).
func (h *Handler) beginRecovery(ctx context.Context) {
	if h.setOnline != nil && h.recoveryDelay > 0 {
		if h.recovering.CompareAndSwap(false, true) {
			h.setOnline(false)
			go h.recover(ctx)
		}
	}
}

// StartupProbe verifies SMTP connectivity at startup. On success it calls
// setOnline(true). On failure it enters the same recovery loop used by
// HandlePacket, ensuring the interface does not claim online until SMTP is
// verified.
func (h *Handler) StartupProbe(ctx context.Context) {
	if h.setOnline == nil {
		return
	}
	if err := h.sender.Probe(ctx); err != nil {
		h.logger.Warn("startup smtp probe failed, entering recovery", "error", err)
		h.beginRecovery(ctx)
		return
	}
	h.logger.Info("startup smtp probe succeeded")
	h.setOnline(true)
}

// recover probes the SMTP server with exponential backoff until it succeeds
// or the context is cancelled. On success it calls setOnline(true).
func (h *Handler) recover(ctx context.Context) {
	defer h.recovering.Store(false)
	backoff := h.recoveryDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if err := h.sender.Probe(ctx); err != nil {
			h.logger.Warn("smtp probe failed, retrying", "delay", backoff, "error", err)
			backoff = min(backoff*2, h.maxRecoveryDelay)
			continue
		}
		h.logger.Info("smtp probe succeeded, interface back online")
		h.setOnline(true)
		return
	}
}
