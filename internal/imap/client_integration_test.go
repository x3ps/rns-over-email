package imap

import (
	"context"
	"io"
	"net"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/x3ps/rns-iface-email/internal/envelope"
)

// startMemServer creates an in-memory IMAP server with one user, one INBOX,
// and returns the listener address and the user (for server-side Append).
// The server is cleaned up when t finishes.
func startMemServer(t *testing.T, caps goimap.CapSet) (addr string, user *imapmemserver.User) {
	t.Helper()

	user = imapmemserver.NewUser("testuser", "testpass")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	memSrv := imapmemserver.New()
	memSrv.AddUser(user)

	if caps == nil {
		caps = goimap.CapSet{}
		caps[goimap.CapIMAP4rev1] = struct{}{}
		caps[goimap.CapIdle] = struct{}{}
	}

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return imapmemserver.NewUserSession(user), nil, nil
		},
		Caps:         caps,
		InsecureAuth: true,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})

	return ln.Addr().String(), user
}

// appendTestMessage uses the server-side user.Append to add a message to a mailbox.
func appendTestMessage(t *testing.T, user *imapmemserver.User, mailbox string, data []byte) {
	t.Helper()
	_, err := user.Append(mailbox, newLiteralReader(data), &goimap.AppendOptions{})
	if err != nil {
		t.Fatalf("Append to %s: %v", mailbox, err)
	}
}

// literalReader implements imap.LiteralReader for user.Append.
type literalReader struct {
	data []byte
	pos  int
}

func newLiteralReader(data []byte) *literalReader {
	return &literalReader{data: data}
}

func (r *literalReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *literalReader) Size() int64 {
	return int64(len(r.data))
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return host, port
}

func TestRealClient_DialLoginSelect(t *testing.T) {
	addr, _ := startMemServer(t, nil)
	host, port := splitAddr(t, addr)

	ctx := context.Background()
	client, err := Dial(ctx, host, port, "none", nil, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Login("testuser", "testpass"); err != nil {
		t.Fatalf("Login: %v", err)
	}

	mbox, err := client.Select("INBOX")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if mbox.UIDValidity == 0 {
		t.Error("UIDValidity should be non-zero after SELECT")
	}
}

func TestRealClient_FetchSince_Empty(t *testing.T) {
	addr, _ := startMemServer(t, nil)
	host, port := splitAddr(t, addr)

	ctx := context.Background()
	client, err := Dial(ctx, host, port, "none", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Login("testuser", "testpass"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX"); err != nil {
		t.Fatal(err)
	}

	var called int
	err = client.FetchSince(0, func(uid uint32, raw []byte) error {
		called++
		return nil
	})
	if err != nil {
		t.Fatalf("FetchSince: %v", err)
	}
	if called != 0 {
		t.Errorf("handler called %d times on empty mailbox, want 0", called)
	}
}

func TestRealClient_FetchSince_Messages(t *testing.T) {
	addr, user := startMemServer(t, nil)
	host, port := splitAddr(t, addr)

	// Append messages server-side.
	msg1 := makeTestEnvelope(t, []byte("pkt-one"))
	msg2 := makeTestEnvelope(t, []byte("pkt-two"))
	appendTestMessage(t, user, "INBOX", msg1)
	appendTestMessage(t, user, "INBOX", msg2)

	ctx := context.Background()
	client, err := Dial(ctx, host, port, "none", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Login("testuser", "testpass"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX"); err != nil {
		t.Fatal(err)
	}

	type msg struct {
		uid uint32
		raw []byte
	}
	var msgs []msg
	err = client.FetchSince(0, func(uid uint32, raw []byte) error {
		msgs = append(msgs, msg{uid: uid, raw: raw})
		return nil
	})
	if err != nil {
		t.Fatalf("FetchSince: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].uid >= msgs[1].uid {
		t.Errorf("UIDs not ascending: %d >= %d", msgs[0].uid, msgs[1].uid)
	}
	// Verify the raw data is decodable.
	for i, m := range msgs {
		if _, err := envelope.Decode(m.raw); err != nil {
			t.Errorf("message %d (UID %d) decode failed: %v", i, m.uid, err)
		}
	}
}

func TestRealClient_FetchSince_FiltersOldUIDs(t *testing.T) {
	addr, user := startMemServer(t, nil)
	host, port := splitAddr(t, addr)

	msg1 := makeTestEnvelope(t, []byte("pkt-1"))
	msg2 := makeTestEnvelope(t, []byte("pkt-2"))
	msg3 := makeTestEnvelope(t, []byte("pkt-3"))
	appendTestMessage(t, user, "INBOX", msg1)
	appendTestMessage(t, user, "INBOX", msg2)
	appendTestMessage(t, user, "INBOX", msg3)

	ctx := context.Background()
	client, err := Dial(ctx, host, port, "none", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Login("testuser", "testpass"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX"); err != nil {
		t.Fatal(err)
	}

	// Fetch with lastUID=2 should only return UID 3+.
	var uids []uint32
	err = client.FetchSince(2, func(uid uint32, raw []byte) error {
		uids = append(uids, uid)
		return nil
	})
	if err != nil {
		t.Fatalf("FetchSince: %v", err)
	}
	for _, uid := range uids {
		if uid <= 2 {
			t.Errorf("got UID %d, want > 2", uid)
		}
	}
	if len(uids) == 0 {
		t.Error("expected at least one message with UID > 2")
	}
}

func TestRealClient_DeleteUIDs(t *testing.T) {
	caps := goimap.CapSet{}
	caps[goimap.CapIMAP4rev1] = struct{}{}
	caps[goimap.CapIdle] = struct{}{}
	caps[goimap.CapUIDPlus] = struct{}{}

	addr, user := startMemServer(t, caps)
	host, port := splitAddr(t, addr)

	msg := makeTestEnvelope(t, []byte("del-pkt"))
	appendTestMessage(t, user, "INBOX", msg)

	ctx := context.Background()
	client, err := Dial(ctx, host, port, "none", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Login("testuser", "testpass"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX"); err != nil {
		t.Fatal(err)
	}

	// Find the UID.
	var uid uint32
	err = client.FetchSince(0, func(u uint32, _ []byte) error {
		uid = u
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if uid == 0 {
		t.Fatal("no message found")
	}

	if err := client.DeleteUIDs([]uint32{uid}); err != nil {
		t.Fatalf("DeleteUIDs: %v", err)
	}

	// Re-fetch: should be empty.
	var count int
	err = client.FetchSince(0, func(_ uint32, _ []byte) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("got %d messages after delete, want 0", count)
	}
}

func TestRealClient_MoveUIDs(t *testing.T) {
	caps := goimap.CapSet{}
	caps[goimap.CapIMAP4rev1] = struct{}{}
	caps[goimap.CapIdle] = struct{}{}
	caps[goimap.CapMove] = struct{}{}
	caps[goimap.CapUIDPlus] = struct{}{}

	addr, user := startMemServer(t, caps)
	host, port := splitAddr(t, addr)

	// Create the destination mailbox.
	if err := user.Create("Archive", nil); err != nil {
		t.Fatal(err)
	}

	msg := makeTestEnvelope(t, []byte("move-pkt"))
	appendTestMessage(t, user, "INBOX", msg)

	ctx := context.Background()
	client, err := Dial(ctx, host, port, "none", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Login("testuser", "testpass"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX"); err != nil {
		t.Fatal(err)
	}

	var uid uint32
	err = client.FetchSince(0, func(u uint32, _ []byte) error {
		uid = u
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if uid == 0 {
		t.Fatal("no message found")
	}

	if err := client.MoveUIDs([]uint32{uid}, "Archive"); err != nil {
		t.Fatalf("MoveUIDs: %v", err)
	}

	// INBOX should be empty.
	var count int
	err = client.FetchSince(0, func(_ uint32, _ []byte) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("INBOX has %d messages after move, want 0", count)
	}
}

func TestRealClient_Idle_ContextCancel(t *testing.T) {
	addr, _ := startMemServer(t, nil)
	host, port := splitAddr(t, addr)

	ctx := context.Background()
	client, err := Dial(ctx, host, port, "none", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Login("testuser", "testpass"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX"); err != nil {
		t.Fatal(err)
	}

	idleCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- client.Idle(idleCtx)
	}()

	cancel()

	err = <-done
	// Idle should return cleanly after cancel (nil or an error is fine).
	_ = err
}

func TestRealClient_HasIdle(t *testing.T) {
	addr, _ := startMemServer(t, nil)
	host, port := splitAddr(t, addr)

	ctx := context.Background()
	client, err := Dial(ctx, host, port, "none", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Login("testuser", "testpass"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX"); err != nil {
		t.Fatal(err)
	}

	// Our test server advertises IDLE.
	if !client.HasIdle() {
		t.Error("HasIdle() = false, want true (server advertises IDLE)")
	}
}

func TestRealClient_Noop(t *testing.T) {
	addr, _ := startMemServer(t, nil)
	host, port := splitAddr(t, addr)

	ctx := context.Background()
	client, err := Dial(ctx, host, port, "none", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Login("testuser", "testpass"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX"); err != nil {
		t.Fatal(err)
	}

	if err := client.Noop(); err != nil {
		t.Fatalf("Noop: %v", err)
	}
}

func TestRealClient_Close(t *testing.T) {
	addr, _ := startMemServer(t, nil)
	host, port := splitAddr(t, addr)

	ctx := context.Background()
	client, err := Dial(ctx, host, port, "none", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := client.Login("testuser", "testpass"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX"); err != nil {
		t.Fatal(err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestRealClient_LoginFailure(t *testing.T) {
	addr, _ := startMemServer(t, nil)
	host, port := splitAddr(t, addr)

	ctx := context.Background()
	client, err := Dial(ctx, host, port, "none", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	err = client.Login("wronguser", "wrongpass")
	if err == nil {
		t.Error("expected login failure, got nil")
	}
}
