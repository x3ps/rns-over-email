package transport

import (
	"net"
	"sync"
	"testing"
	"time"
)

func TestTimeoutConnReadSetsDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	tc := NewTimeoutConn(client, 5*time.Second)

	go func() {
		_, _ = server.Write([]byte("hello"))
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
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

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
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	// Start with a very short timeout, then extend it via SetTimeout.
	tc := NewTimeoutConn(client, 1*time.Millisecond)
	tc.SetTimeout(5 * time.Second)

	go func() {
		_, _ = server.Write([]byte("x"))
	}()

	buf := make([]byte, 1)
	if _, err := tc.Read(buf); err != nil {
		t.Fatalf("Read after SetTimeout(5s) failed: %v", err)
	}
}

func TestTimeoutConnReadTimeout(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

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

// TestTimeoutConnConcurrentSetTimeoutRead verifies no data race when
// SetTimeout is called concurrently with Read (e.g. IMAP IDLE timeout change).
// Run with -race to detect races.
func TestTimeoutConnConcurrentSetTimeoutRead(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	tc := NewTimeoutConn(client, 5*time.Second)

	var wg sync.WaitGroup

	// Writer goroutine: feed data so reads don't block.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := server.Write([]byte("x")); err != nil {
				return
			}
		}
	}()

	// Reader goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1)
		for i := 0; i < 100; i++ {
			_, _ = tc.Read(buf)
		}
	}()

	// Concurrent SetTimeout calls.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			tc.SetTimeout(time.Duration(i+1) * time.Second)
		}
	}()

	wg.Wait()
}

func TestTimeoutConnHardDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	tc := NewTimeoutConn(client, 5*time.Second)
	tc.SetHardDeadline(time.Now().Add(100 * time.Millisecond))

	buf := make([]byte, 1)
	start := time.Now()
	_, err := tc.Read(buf) // no writer — should timeout at hard deadline
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	netErr, ok := err.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Errorf("expected network timeout error, got %T: %v", err, err)
	}
	if elapsed > time.Second {
		t.Errorf("Read took %v, expected ~100ms (hard deadline)", elapsed)
	}
}

func TestTimeoutConnHardDeadlinePerOpShorter(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	// Per-op timeout is 1ms, hard deadline is far in the future.
	tc := NewTimeoutConn(client, 1*time.Millisecond)
	tc.SetHardDeadline(time.Now().Add(10 * time.Second))

	buf := make([]byte, 1)
	start := time.Now()
	_, err := tc.Read(buf)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > time.Second {
		t.Errorf("Read took %v, expected ~1ms (per-op shorter)", elapsed)
	}
}

func TestTimeoutConnClearHardDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	tc := NewTimeoutConn(client, 5*time.Second)
	tc.SetHardDeadline(time.Now().Add(1 * time.Millisecond))
	time.Sleep(5 * time.Millisecond) // let hard deadline pass

	// Clear the hard deadline.
	tc.SetHardDeadline(time.Time{})

	// Now write data to the server side so Read succeeds.
	go func() {
		_, _ = server.Write([]byte("x"))
	}()

	buf := make([]byte, 1)
	_, err := tc.Read(buf)
	if err != nil {
		t.Fatalf("Read after clearing hard deadline should succeed, got: %v", err)
	}
}

// TestTimeoutConnConcurrentSetHardDeadline verifies no data race when
// SetHardDeadline is called concurrently with Read.
// Run with -race to detect races.
func TestTimeoutConnConcurrentSetHardDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	tc := NewTimeoutConn(client, 5*time.Second)

	var wg sync.WaitGroup

	// Writer goroutine: feed data so reads don't block.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := server.Write([]byte("x")); err != nil {
				return
			}
		}
	}()

	// Reader goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1)
		for i := 0; i < 100; i++ {
			_, _ = tc.Read(buf)
		}
	}()

	// Concurrent SetHardDeadline calls.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			tc.SetHardDeadline(time.Now().Add(time.Duration(i+1) * time.Second))
		}
	}()

	wg.Wait()
}

func TestTimeoutConnWriteTimeout(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	// 1ms timeout with no reader — Write must return a timeout error.
	// net.Pipe has no internal buffer; without a reader, the write blocks.
	tc := NewTimeoutConn(client, 1*time.Millisecond)

	// Write enough data to ensure it can't complete instantly.
	_, err := tc.Write([]byte("hello"))
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	netErr, ok := err.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Errorf("expected network timeout error, got %T: %v", err, err)
	}
}

// TestTimeoutConnConcurrentReadWrite verifies that concurrent Read and Write
// use direction-specific deadlines and don't interfere with each other.
// Run with -race to detect races.
func TestTimeoutConnConcurrentReadWrite(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	tc := NewTimeoutConn(client, 5*time.Second)

	var wg sync.WaitGroup
	const iterations = 50

	// Server side: read what the client writes, and write data for the client to read.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 5)
		for i := 0; i < iterations; i++ {
			_, _ = server.Read(buf)
			_, _ = server.Write([]byte("reply"))
		}
	}()

	// Client reader.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 5)
		for i := 0; i < iterations; i++ {
			_, _ = tc.Read(buf)
		}
	}()

	// Client writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, _ = tc.Write([]byte("hello"))
		}
	}()

	wg.Wait()
}
