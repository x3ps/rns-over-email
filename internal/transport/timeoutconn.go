package transport

import (
	"net"
	"time"
)

// TimeoutConn wraps a net.Conn and sets a deadline before each Read/Write.
type TimeoutConn struct {
	net.Conn
	timeout time.Duration
}

// NewTimeoutConn creates a conn wrapper that sets per-operation deadlines.
func NewTimeoutConn(c net.Conn, timeout time.Duration) *TimeoutConn {
	return &TimeoutConn{Conn: c, timeout: timeout}
}

// SetTimeout changes the per-operation deadline duration.
// Safe to call between operations on single-threaded IMAP connections.
func (c *TimeoutConn) SetTimeout(d time.Duration) {
	c.timeout = d
}

func (c *TimeoutConn) Read(b []byte) (int, error) {
	if err := c.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *TimeoutConn) Write(b []byte) (int, error) {
	if err := c.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}
