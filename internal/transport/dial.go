package transport

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"time"
)

// DialTCP opens a TCP connection to host:port with the given TLS mode and wraps
// it in a TimeoutConn. The caller is responsible for wrapping errors with a
// context-appropriate prefix (e.g. "smtp dial tls:", "imap dial:").
//
// tlsMode values:
//   - "tls":      TLS from the start (tls.DialWithDialer)
//   - "starttls": plain TCP; STARTTLS negotiation is done by the caller
//   - "none" or anything else: plain TCP, no TLS
func DialTCP(ctx context.Context, host string, port int, tlsMode string, dialTimeout, ioTimeout time.Duration) (*TimeoutConn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := &net.Dialer{Timeout: dialTimeout}

	switch tlsMode {
	case "tls":
		rawConn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		if deadline, ok := ctx.Deadline(); ok {
			rawConn.SetDeadline(deadline)
		}
		tlsConn := tls.Client(rawConn, &tls.Config{ServerName: host})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, err
		}
		rawConn.SetDeadline(time.Time{})
		return NewTimeoutConn(tlsConn, ioTimeout), nil
	default: // "starttls" or "none"
		rawConn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		return NewTimeoutConn(rawConn, ioTimeout), nil
	}
}
