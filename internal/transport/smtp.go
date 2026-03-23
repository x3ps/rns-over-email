package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// ErrDataOutcomeUnknown wraps non-deterministic errors that occur after
// the SMTP DATA dot-terminator has been sent. The server may or may not
// have accepted the message. Callers must not retry to preserve
// at-most-once semantics.
type ErrDataOutcomeUnknown struct{ Err error }

func (e *ErrDataOutcomeUnknown) Error() string { return e.Err.Error() }
func (e *ErrDataOutcomeUnknown) Unwrap() error { return e.Err }

const (
	smtpDialTimeout = 30 * time.Second
	smtpIOTimeout   = 60 * time.Second
)

// SMTPSender sends email messages via SMTP.
type SMTPSender struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	TLSMode  string // "tls", "starttls", "none"
	Logger   *slog.Logger
}

// connect dials the SMTP server, creates a client with the configured TLS
// mode, and authenticates. The caller must Close the returned client and
// clear the hard deadline on the returned TimeoutConn when done.
func (s *SMTPSender) connect(ctx context.Context, label string) (*smtp.Client, *TimeoutConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	tc, dialErr := DialTCP(ctx, s.Host, s.Port, s.TLSMode, smtpDialTimeout, smtpIOTimeout)
	if dialErr != nil {
		if s.TLSMode == "tls" {
			return nil, nil, fmt.Errorf("%s dial tls: %w", label, dialErr)
		}
		return nil, nil, fmt.Errorf("%s dial: %w", label, dialErr)
	}

	// Propagate context deadline to the connection so STARTTLS, AUTH, and
	// subsequent SMTP commands respect the caller's timeout budget.
	if deadline, ok := ctx.Deadline(); ok {
		tc.SetHardDeadline(deadline)
	}

	var c *smtp.Client
	if s.TLSMode == "starttls" {
		var err error
		c, err = smtp.NewClientStartTLS(tc, &tls.Config{ServerName: s.Host})
		if err != nil {
			_ = tc.Close()
			return nil, nil, fmt.Errorf("%s starttls: %w", label, err)
		}
	} else {
		c = smtp.NewClient(tc)
	}

	if err := ctx.Err(); err != nil {
		_ = c.Close()
		return nil, nil, err
	}

	var auth sasl.Client
	switch {
	case c.SupportsAuth("PLAIN"):
		auth = sasl.NewPlainClient("", s.Username, s.Password)
	case c.SupportsAuth("LOGIN"):
		auth = sasl.NewLoginClient(s.Username, s.Password)
	default:
		if s.Logger != nil {
			s.Logger.Warn("server advertises no known auth mechanisms, attempting PLAIN as compatibility fallback")
		}
		auth = sasl.NewPlainClient("", s.Username, s.Password)
	}
	if err := c.Auth(auth); err != nil {
		_ = c.Close()
		return nil, nil, fmt.Errorf("%s auth: %w", label, err)
	}

	return c, tc, nil
}

func (s *SMTPSender) Send(ctx context.Context, to string, msg []byte) error {
	c, tc, err := s.connect(ctx, "smtp")
	if err != nil {
		return err
	}
	defer func() {
		tc.SetHardDeadline(time.Time{})
		_ = c.Close()
	}()

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := c.Mail(s.From, nil); err != nil {
		return fmt.Errorf("smtp mail: %w", err)
	}
	if err := c.Rcpt(to, nil); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}

	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := wc.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := wc.Close(); err != nil {
		closeErr := fmt.Errorf("smtp data close: %w", err)
		// Classify: go-smtp's CloseWithResponse() returns *smtp.SMTPError
		// when the server explicitly responds with 4xx/5xx. It returns a
		// raw net/io error on timeout, EOF, or connection reset.
		var smtpErr *smtp.SMTPError
		if errors.As(err, &smtpErr) {
			// Deterministic rejection — safe to retry.
			return closeErr
		}
		// Non-SMTP error: dot-terminator was sent, but we never received
		// the server's final response. Outcome unknown.
		return &ErrDataOutcomeUnknown{Err: closeErr}
	}

	_ = c.Quit() // best-effort; message already committed by DATA
	return nil
}

// Probe dials the SMTP server, authenticates, and quits without sending any
// message. It is used to verify that the server is reachable and credentials
// are valid before bringing the interface back online.
func (s *SMTPSender) Probe(ctx context.Context) error {
	c, tc, err := s.connect(ctx, "smtp probe")
	if err != nil {
		return err
	}
	defer func() {
		tc.SetHardDeadline(time.Time{})
		_ = c.Close()
	}()

	_ = c.Quit()
	return nil
}
