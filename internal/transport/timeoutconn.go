package transport

import (
	"net"
	"sync/atomic"
	"time"
)

// TimeoutConn wraps a net.Conn and sets a deadline before each Read/Write.
type TimeoutConn struct {
	net.Conn
	timeout      atomic.Int64 // nanoseconds; per-op deadline
	hardDeadline atomic.Int64 // UnixNano; 0 = no hard deadline
}

// NewTimeoutConn creates a conn wrapper that sets per-operation deadlines.
func NewTimeoutConn(c net.Conn, timeout time.Duration) *TimeoutConn {
	tc := &TimeoutConn{Conn: c}
	tc.timeout.Store(int64(timeout))
	return tc
}

// SetTimeout changes the per-operation deadline duration.
// Safe to call concurrently with Read/Write (e.g. during IMAP IDLE).
func (c *TimeoutConn) SetTimeout(d time.Duration) {
	c.timeout.Store(int64(d))
}

// SetHardDeadline sets an absolute deadline that caps per-operation deadlines.
// Pass a zero Time to clear the hard deadline.
// Safe to call concurrently with Read/Write.
func (c *TimeoutConn) SetHardDeadline(t time.Time) {
	if t.IsZero() {
		c.hardDeadline.Store(0)
	} else {
		c.hardDeadline.Store(t.UnixNano())
	}
}

// effectiveDeadline returns the earlier of the per-op and hard deadlines.
func (c *TimeoutConn) effectiveDeadline() time.Time {
	d := time.Duration(c.timeout.Load())
	deadline := time.Now().Add(d)
	if hard := c.hardDeadline.Load(); hard > 0 {
		if hardTime := time.Unix(0, hard); hardTime.Before(deadline) {
			deadline = hardTime
		}
	}
	return deadline
}

func (c *TimeoutConn) Read(b []byte) (int, error) {
	if err := c.SetReadDeadline(c.effectiveDeadline()); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *TimeoutConn) Write(b []byte) (int, error) {
	if err := c.SetWriteDeadline(c.effectiveDeadline()); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}
