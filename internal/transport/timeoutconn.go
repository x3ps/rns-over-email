package transport

import (
	"net"
	"sync/atomic"
	"time"
)

// TimeoutConn wraps a net.Conn and sets a deadline before each Read/Write.
type TimeoutConn struct {
	net.Conn
	timeout atomic.Int64 // nanoseconds
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

func (c *TimeoutConn) Read(b []byte) (int, error) {
	d := time.Duration(c.timeout.Load())
	if err := c.SetReadDeadline(time.Now().Add(d)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *TimeoutConn) Write(b []byte) (int, error) {
	d := time.Duration(c.timeout.Load())
	if err := c.SetWriteDeadline(time.Now().Add(d)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}
