package transport

import (
	"net"
	"testing"
	"time"
)

func TestTimeoutConnReadSetsDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	tc := NewTimeoutConn(client, 5*time.Second)

	go func() {
		server.Write([]byte("hello"))
	}()

	buf := make([]byte, 5)
	n, err := tc.Read(buf)
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Errorf("Read data = %q, want hello", buf[:n])
	}
}

func TestTimeoutConnWriteSetsDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	tc := NewTimeoutConn(client, 5*time.Second)

	done := make(chan error, 1)
	go func() {
		_, err := tc.Write([]byte("world"))
		done <- err
	}()

	buf := make([]byte, 5)
	n, err := server.Read(buf)
	if err != nil {
		t.Fatalf("server read error: %v", err)
	}
	if string(buf[:n]) != "world" {
		t.Errorf("received %q, want world", buf[:n])
	}
	if err := <-done; err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
}

func TestTimeoutConnSetTimeout(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Start with a very short timeout, then extend it via SetTimeout.
	tc := NewTimeoutConn(client, 1*time.Millisecond)
	tc.SetTimeout(5 * time.Second)

	go func() {
		server.Write([]byte("x"))
	}()

	buf := make([]byte, 1)
	if _, err := tc.Read(buf); err != nil {
		t.Fatalf("Read after SetTimeout(5s) failed: %v", err)
	}
}

func TestTimeoutConnReadTimeout(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// 1ms timeout with no writer — Read must return a timeout error.
	tc := NewTimeoutConn(client, 1*time.Millisecond)

	buf := make([]byte, 1)
	_, err := tc.Read(buf)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	netErr, ok := err.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Errorf("expected network timeout error, got %T: %v", err, err)
	}
}
