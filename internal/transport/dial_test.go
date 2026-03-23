package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"testing"
	"time"
)

// selfSignedCert generates a self-signed TLS certificate for testing.
func selfSignedCert(t *testing.T, host string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}
}

// testTLSConfig returns a client tls.Config that trusts the given server cert.
func testTLSConfig(t *testing.T, cert tls.Certificate, serverName string) *tls.Config {
	t.Helper()
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(x509Cert)
	return &tls.Config{
		ServerName: serverName,
		RootCAs:    pool,
	}
}

func TestDialTCPTLSSuccess(t *testing.T) {
	cert := selfSignedCert(t, "localhost")
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 16)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n])
	}()

	host, port := splitHostPort(t, ln.Addr().String())
	clientCfg := testTLSConfig(t, cert, "localhost")

	tc, err := DialTCP(context.Background(), host, port, "tls", 5*time.Second, 5*time.Second, clientCfg)
	if err != nil {
		t.Fatalf("DialTCP tls: %v", err)
	}
	defer func() { _ = tc.Close() }()

	// Verify the connection is usable.
	if _, err := tc.Write([]byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 4)
	n, err := tc.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "ping" {
		t.Errorf("got %q, want ping", buf[:n])
	}
}

func TestDialTCPNoneMode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 16)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n])
	}()

	host, port := splitHostPort(t, ln.Addr().String())
	tc, err := DialTCP(context.Background(), host, port, "none", 5*time.Second, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("DialTCP none: %v", err)
	}
	defer func() { _ = tc.Close() }()

	if _, err := tc.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 2)
	n, err := tc.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "hi" {
		t.Errorf("got %q, want hi", buf[:n])
	}
}

func TestDialTCPStartTLSReturnsPlainConn(t *testing.T) {
	// starttls mode should return a plain TCP connection (caller does STARTTLS).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte("ok"))
	}()

	host, port := splitHostPort(t, ln.Addr().String())
	tc, err := DialTCP(context.Background(), host, port, "starttls", 5*time.Second, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("DialTCP starttls: %v", err)
	}
	defer func() { _ = tc.Close() }()

	buf := make([]byte, 2)
	n, err := tc.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "ok" {
		t.Errorf("got %q, want ok", buf[:n])
	}
}

func TestDialTCPTLSHandshakeFailure(t *testing.T) {
	cert := selfSignedCert(t, "localhost")
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			_ = conn.Close()
		}
	}()

	host, port := splitHostPort(t, ln.Addr().String())
	// Use wrong ServerName → handshake failure.
	wrongCfg := testTLSConfig(t, cert, "wrong-host.example.com")

	_, err = DialTCP(context.Background(), host, port, "tls", 5*time.Second, 5*time.Second, wrongCfg)
	if err == nil {
		t.Fatal("expected TLS handshake error, got nil")
	}
}

func TestDialTCPCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := DialTCP(ctx, "127.0.0.1", 1, "none", 5*time.Second, 5*time.Second, nil)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

func TestDialTCPTLSDefaultConfig(t *testing.T) {
	// When tlsCfg is nil, DialTCP uses ServerName=host. With a self-signed
	// cert, the default system CA pool won't trust it → handshake error.
	cert := selfSignedCert(t, "localhost")
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			_ = conn.Close()
		}
	}()

	host, port := splitHostPort(t, ln.Addr().String())
	// nil tlsCfg → uses default (no custom CA) → should fail with self-signed cert
	_, err = DialTCP(context.Background(), host, port, "tls", 5*time.Second, 5*time.Second, nil)
	if err == nil {
		t.Fatal("expected TLS error with default config and self-signed cert, got nil")
	}
}
