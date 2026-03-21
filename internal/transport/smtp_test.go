package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// testMessage records a received email.
type testMessage struct {
	From string
	To   string
	Data []byte
}

// testBackend implements smtp.Backend for testing.
type testBackend struct {
	mu       sync.Mutex
	messages []testMessage
}

func (b *testBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &testSession{backend: b}, nil
}

type testSession struct {
	backend  *testBackend
	from     string
	to       string
	username string
	password string
}

// AuthMechanisms implements smtp.AuthSession.
func (s *testSession) AuthMechanisms() []string {
	return []string{"PLAIN"}
}

// Auth implements smtp.AuthSession.
func (s *testSession) Auth(mech string) (sasl.Server, error) {
	if mech != "PLAIN" {
		return nil, fmt.Errorf("unsupported mechanism: %s", mech)
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if username != "testuser" || password != "testpass" {
			return fmt.Errorf("invalid credentials")
		}
		s.username = username
		s.password = password
		return nil
	}), nil
}

func (s *testSession) Mail(from string, opts *smtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *testSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	s.to = to
	return nil
}

func (s *testSession) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	s.backend.messages = append(s.backend.messages, testMessage{
		From: s.from,
		To:   s.to,
		Data: data,
	})
	return nil
}

func (s *testSession) Reset() {}

func (s *testSession) Logout() error { return nil }

func startTestServer(t *testing.T, backend *testBackend) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := smtp.NewServer(backend)
	srv.Domain = "localhost"
	srv.AllowInsecureAuth = true

	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
	})

	return ln.Addr().String()
}

func TestSMTPSendSuccess(t *testing.T) {
	backend := &testBackend{}
	addr := startTestServer(t, backend)
	host, port := splitHostPort(t, addr)

	sender := &SMTPSender{
		Host:     host,
		Port:     port,
		Username: "testuser",
		Password: "testpass",
		From:     "sender@test.com",
		TLSMode:  "none",
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	body := []byte("Subject: test\r\n\r\nHello, world!")
	if err := sender.Send(context.Background(), "receiver@test.com", body); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(backend.messages))
	}
	msg := backend.messages[0]
	if msg.From != "sender@test.com" {
		t.Errorf("from = %q, want sender@test.com", msg.From)
	}
	if msg.To != "receiver@test.com" {
		t.Errorf("to = %q, want receiver@test.com", msg.To)
	}
	if !bytes.HasPrefix(msg.Data, body) {
		t.Errorf("data mismatch:\n  got:  %q\n  want prefix: %q", msg.Data, body)
	}
}

func TestSMTPAuthFailure(t *testing.T) {
	backend := &testBackend{}
	addr := startTestServer(t, backend)
	host, port := splitHostPort(t, addr)

	sender := &SMTPSender{
		Host:     host,
		Port:     port,
		Username: "wrong",
		Password: "wrong",
		From:     "sender@test.com",
		TLSMode:  "none",
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err := sender.Send(context.Background(), "receiver@test.com", []byte("test"))
	if err == nil {
		t.Error("expected auth error, got nil")
	}
}

func TestSMTPContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	sender := &SMTPSender{
		Host:     "127.0.0.1",
		Port:     1,
		Username: "u",
		Password: "p",
		From:     "f@t.com",
		TLSMode:  "none",
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err := sender.Send(ctx, "to@test.com", []byte("test"))
	if err == nil {
		t.Error("expected context error, got nil")
	}
}

func TestSMTPQuitFailureDoesNotReturnError(t *testing.T) {
	// Raw minimal SMTP server: sends 250 after DATA then closes without
	// responding to QUIT, simulating a network drop post-DATA.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	received := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		write := func(s string) { conn.Write([]byte(s + "\r\n")) }
		buf := make([]byte, 4096)
		readline := func() string {
			var line []byte
			for {
				n, _ := conn.Read(buf[:1])
				if n == 0 {
					break
				}
				line = append(line, buf[0])
				if len(line) >= 2 && line[len(line)-2] == '\r' && line[len(line)-1] == '\n' {
					return strings.TrimRight(string(line), "\r\n")
				}
			}
			return string(line)
		}

		write("220 localhost ESMTP test")
		readline() // EHLO
		write("250-localhost")
		write("250 AUTH PLAIN")
		readline() // AUTH PLAIN <creds>
		write("235 ok")
		readline() // MAIL FROM
		write("250 ok")
		readline() // RCPT TO
		write("250 ok")
		readline() // DATA
		write("354 go ahead")
		for {
			l := readline()
			if l == "." {
				break
			}
		}
		write("250 ok")
		received <- struct{}{}
		// Close without responding to QUIT — simulates post-DATA network drop.
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	sender := &SMTPSender{
		Host:     host,
		Port:     port,
		Username: "testuser",
		Password: "testpass",
		From:     "sender@test.com",
		TLSMode:  "none",
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err = sender.Send(context.Background(), "receiver@test.com", []byte("Subject: test\r\n\r\nQuit failure test"))
	// Message was committed by DATA; QUIT failure must not propagate.
	if err != nil {
		t.Errorf("Send returned error after DATA success + QUIT failure: %v", err)
	}

	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Error("server did not receive message")
	}
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func TestSMTPProbeSuccess(t *testing.T) {
	backend := &testBackend{}
	addr := startTestServer(t, backend)
	host, port := splitHostPort(t, addr)

	sender := &SMTPSender{
		Host:     host,
		Port:     port,
		Username: "testuser",
		Password: "testpass",
		From:     "sender@test.com",
		TLSMode:  "none",
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	if err := sender.Probe(context.Background()); err != nil {
		t.Fatalf("Probe failed: %v", err)
	}

	// Probe should not have sent any messages.
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.messages) != 0 {
		t.Errorf("expected 0 messages after probe, got %d", len(backend.messages))
	}
}

func TestSMTPProbeAuthFailure(t *testing.T) {
	backend := &testBackend{}
	addr := startTestServer(t, backend)
	host, port := splitHostPort(t, addr)

	sender := &SMTPSender{
		Host:     host,
		Port:     port,
		Username: "wrong",
		Password: "wrong",
		From:     "sender@test.com",
		TLSMode:  "none",
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err := sender.Probe(context.Background())
	if err == nil {
		t.Error("expected auth error from Probe, got nil")
	}
}

// errTestSession is a testSession variant that returns errors at configurable stages.
type errTestSession struct {
	testSession
	mailErr error
	rcptErr error
	dataErr error
}

func (s *errTestSession) Mail(from string, opts *smtp.MailOptions) error {
	if s.mailErr != nil {
		return s.mailErr
	}
	return s.testSession.Mail(from, opts)
}

func (s *errTestSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	if s.rcptErr != nil {
		return s.rcptErr
	}
	return s.testSession.Rcpt(to, opts)
}

func (s *errTestSession) Data(r io.Reader) error {
	if s.dataErr != nil {
		return s.dataErr
	}
	return s.testSession.Data(r)
}

// errBackend creates sessions that fail at specific SMTP stages.
type errBackend struct {
	mailErr error
	rcptErr error
	dataErr error
}

func (b *errBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &errTestSession{
		testSession: testSession{backend: &testBackend{}},
		mailErr:     b.mailErr,
		rcptErr:     b.rcptErr,
		dataErr:     b.dataErr,
	}, nil
}

func startErrTestServer(t *testing.T, backend smtp.Backend) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := smtp.NewServer(backend)
	srv.Domain = "localhost"
	srv.AllowInsecureAuth = true
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func TestSMTPSendErrors(t *testing.T) {
	tests := []struct {
		name    string
		backend *errBackend
		errMsg  string
	}{
		{
			name:    "MAIL FROM error",
			backend: &errBackend{mailErr: fmt.Errorf("rejected sender")},
			errMsg:  "smtp mail",
		},
		{
			name:    "RCPT TO error",
			backend: &errBackend{rcptErr: fmt.Errorf("rejected recipient")},
			errMsg:  "smtp rcpt",
		},
		{
			name:    "DATA error",
			backend: &errBackend{dataErr: fmt.Errorf("rejected data")},
			errMsg:  "smtp data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := startErrTestServer(t, tt.backend)
			host, port := splitHostPort(t, addr)

			sender := &SMTPSender{
				Host:     host,
				Port:     port,
				Username: "testuser",
				Password: "testpass",
				From:     "sender@test.com",
				TLSMode:  "none",
				Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
			}

			err := sender.Send(context.Background(), "receiver@test.com", []byte("Subject: test\r\n\r\nHello"))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.errMsg)
			}
		})
	}
}
