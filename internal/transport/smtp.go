package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

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
// mode, and authenticates. The caller must Close the returned client.
func (s *SMTPSender) connect(ctx context.Context, label string) (*smtp.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	tc, dialErr := DialTCP(ctx, s.Host, s.Port, s.TLSMode, smtpDialTimeout, smtpIOTimeout)
	if dialErr != nil {
		if s.TLSMode == "tls" {
			return nil, fmt.Errorf("%s dial tls: %w", label, dialErr)
		}
		return nil, fmt.Errorf("%s dial: %w", label, dialErr)
	}

	var c *smtp.Client
	if s.TLSMode == "starttls" {
		var err error
		c, err = smtp.NewClientStartTLS(tc, &tls.Config{ServerName: s.Host})
		if err != nil {
			tc.Close()
			return nil, fmt.Errorf("%s starttls: %w", label, err)
		}
	} else {
		c = smtp.NewClient(tc)
	}

	if err := ctx.Err(); err != nil {
		c.Close()
		return nil, err
	}

	auth := sasl.NewPlainClient("", s.Username, s.Password)
	if err := c.Auth(auth); err != nil {
		c.Close()
		return nil, fmt.Errorf("%s auth: %w", label, err)
	}

	return c, nil
}

func (s *SMTPSender) Send(ctx context.Context, to string, msg []byte) error {
	c, err := s.connect(ctx, "smtp")
	if err != nil {
		return err
	}
	defer c.Close()

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
		return fmt.Errorf("smtp data close: %w", err)
	}

	_ = c.Quit() // best-effort; message already committed by DATA
	return nil
}

// Probe dials the SMTP server, authenticates, and quits without sending any
// message. It is used to verify that the server is reachable and credentials
// are valid before bringing the interface back online.
func (s *SMTPSender) Probe(ctx context.Context) error {
	c, err := s.connect(ctx, "smtp probe")
	if err != nil {
		return err
	}
	defer c.Close()

	_ = c.Quit()
	return nil
}
