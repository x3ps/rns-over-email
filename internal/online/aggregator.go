package online

import (
	"log/slog"
	"sync"
)

// Aggregator combines independent IMAP and SMTP health signals into a single
// online/offline state for the RNS pipe interface.
//
// Semantics: combined = imap && smtp
//
// Both transport sub-components must be healthy for the interface to
// accept/deliver packets. The interface stays offline until IMAP connects, even
// if SMTP is already available. This is correct for a bidirectional transport
// where "healthy" means both directions work.
//
// If a future requirement needs per-direction health (e.g. send-only while IMAP
// is down), this aggregator model is insufficient and would need a richer
// direction/queue model — a different scope of work.
type Aggregator struct {
	mu       sync.Mutex
	imap     bool
	smtp     bool
	last     bool
	callback func(bool)
	logger   *slog.Logger
}

// NewAggregator creates an Aggregator with initial state imap=false, smtp=false.
// Both transport sub-components must be verified before the interface claims
// online. It calls callback(false) immediately to push the initial combined=false
// state into iface.SetOnline BEFORE iface.Start(), closing the startup window
// where go-rns-pipe defaults transportOnline=true but neither IMAP nor SMTP has
// been verified yet.
func NewAggregator(callback func(bool), logger *slog.Logger) *Aggregator {
	a := &Aggregator{
		callback: callback,
		logger:   logger.With("component", "online-aggregator"),
		last:     true, // force initial callback(false) to fire as a transition
	}
	// Push initial combined=false before anything starts.
	a.callback(false)
	a.last = false
	return a
}

// SetIMAP updates the IMAP component state.
func (a *Aggregator) SetIMAP(online bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.imap = online
	a.update("imap", online)
}

// SetSMTP updates the SMTP component state.
func (a *Aggregator) SetSMTP(online bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.smtp = online
	a.update("smtp", online)
}

// update computes the combined state and calls the callback on transition.
// Must be called with mu held.
func (a *Aggregator) update(component string, value bool) {
	combined := a.imap && a.smtp
	a.logger.Info("component state changed",
		"component", component, "online", value,
		"combined", combined)
	if combined != a.last {
		a.last = combined
		a.callback(combined)
	}
}
