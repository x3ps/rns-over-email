package imap

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/x3ps/rns-iface-email/internal/transport"
)

const (
	imapDialTimeout = 30 * time.Second
	imapIOTimeout   = 60 * time.Second
	imapIdleTimeout = 30 * time.Minute
)

// MailboxState holds info returned after selecting a mailbox.
type MailboxState struct {
	UIDValidity uint32
}

// Client is a thin interface over an IMAP connection for testability.
type Client interface {
	Login(username, password string) error
	Select(mailbox string) (MailboxState, error)
	FetchSince(lastUID uint32, handler func(uid uint32, raw []byte) error) error
	Idle(ctx context.Context) error
	HasIdle() bool
	Noop() error
	MoveUIDs(uids []uint32, dest string) error
	DeleteUIDs(uids []uint32) error
	Close() error
}

// realClient wraps *imapclient.Client and the underlying TimeoutConn.
type realClient struct {
	c  *imapclient.Client
	tc *transport.TimeoutConn
}

// Dial creates a real IMAP client with the given TLS mode and host.
func Dial(ctx context.Context, host string, port int, tlsMode string, onMailbox func()) (*realClient, error) {
	opts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil && onMailbox != nil {
					onMailbox()
				}
			},
		},
	}

	tc, dialErr := transport.DialTCP(ctx, host, port, tlsMode, imapDialTimeout, imapIOTimeout)
	if dialErr != nil {
		if tlsMode == "tls" {
			return nil, fmt.Errorf("imap dial tls: %w", dialErr)
		}
		return nil, fmt.Errorf("imap dial: %w", dialErr)
	}

	var c *imapclient.Client
	if tlsMode == "starttls" {
		var err error
		opts.TLSConfig = &tls.Config{ServerName: host}
		c, err = imapclient.NewStartTLS(tc, opts)
		if err != nil {
			_ = tc.Close()
			return nil, fmt.Errorf("imap starttls: %w", err)
		}
	} else {
		c = imapclient.New(tc, opts)
	}

	return &realClient{c: c, tc: tc}, nil
}

func (r *realClient) Login(username, password string) error {
	return r.c.Login(username, password).Wait()
}

func (r *realClient) Select(mailbox string) (MailboxState, error) {
	data, err := r.c.Select(mailbox, nil).Wait()
	if err != nil {
		return MailboxState{}, err
	}
	return MailboxState{
		UIDValidity: data.UIDValidity,
	}, nil
}

func (r *realClient) FetchSince(lastUID uint32, handler func(uid uint32, raw []byte) error) (retErr error) {
	// UID FETCH lastUID+1:* (UID BODY.PEEK[])
	uidSet := goimap.UIDSet{goimap.UIDRange{
		Start: goimap.UID(lastUID + 1),
		Stop:  0, // 0 = * (largest)
	}}
	fetchOpts := &goimap.FetchOptions{
		UID: true,
		BodySection: []*goimap.FetchItemBodySection{
			{Peek: true},
		},
	}
	cmd := r.c.Fetch(uidSet, fetchOpts)
	defer func() {
		if closeErr := cmd.Close(); closeErr != nil && retErr == nil {
			retErr = closeErr
		}
	}()

	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			return fmt.Errorf("fetch collect: %w", err)
		}
		if uint32(buf.UID) <= lastUID {
			// Server returned UID <= lastUID (can happen with UID FETCH *).
			continue
		}
		var raw []byte
		for _, bs := range buf.BodySection {
			raw = bs.Bytes
			break
		}
		if raw == nil {
			continue
		}
		if err := handler(uint32(buf.UID), raw); err != nil {
			return err
		}
	}
	return nil
}

func (r *realClient) Idle(ctx context.Context) error {
	// Extend timeout for IDLE: server may not send anything for minutes.
	// Per RFC 2177, IDLE should be maintained up to 29 minutes.
	r.tc.SetTimeout(imapIdleTimeout)
	defer r.tc.SetTimeout(imapIOTimeout) // safety net for early returns; also set explicitly before Close/Wait

	idleCmd, err := r.c.Idle()
	if err != nil {
		return fmt.Errorf("idle start: %w", err)
	}

	<-ctx.Done()

	r.tc.SetTimeout(imapIOTimeout)
	if err := idleCmd.Close(); err != nil {
		return fmt.Errorf("idle close: %w", err)
	}
	return idleCmd.Wait()
}

func (r *realClient) HasIdle() bool {
	caps := r.c.Caps()
	_, ok := caps[goimap.CapIdle]
	return ok
}

func (r *realClient) Noop() error {
	return r.c.Noop().Wait()
}

func (r *realClient) MoveUIDs(uids []uint32, dest string) error {
	if len(uids) == 0 {
		return nil
	}
	uidSet := uidsToSet(uids)
	_, err := r.c.Move(uidSet, dest).Wait()
	if err != nil {
		return fmt.Errorf("move uids: %w", err)
	}
	return nil
}

func (r *realClient) DeleteUIDs(uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	uidSet := uidsToSet(uids)

	// Store returns *FetchCommand; Close() drains pending FETCH responses (none
	// expected when Silent=true) and then calls wait() to block until the server
	// sends the final tagged OK/NO/BAD — semantically identical to Wait() here.
	storeCmd := r.c.Store(uidSet, &goimap.StoreFlags{
		Op:     goimap.StoreFlagsAdd,
		Silent: true,
		Flags:  []goimap.Flag{goimap.FlagDeleted},
	}, nil)
	if err := storeCmd.Close(); err != nil {
		return fmt.Errorf("store deleted flag: %w", err)
	}

	caps := r.c.Caps()
	if _, ok := caps[goimap.CapUIDPlus]; ok {
		// UID EXPUNGE (RFC 4315) removes only the specific UIDs we flagged.
		if err := r.c.UIDExpunge(uidSet).Close(); err != nil {
			return fmt.Errorf("uid expunge: %w", err)
		}
	} else {
		// Without UIDPLUS the plain EXPUNGE removes *all* \Deleted messages in
		// the mailbox, not just ours.  This is safe for exclusive access but can
		// cause data loss under concurrent access (another client's \Deleted
		// messages will also be expunged).  Prefer a server with UIDPLUS support
		// when multiple clients share the same mailbox.
		if err := r.c.Expunge().Close(); err != nil {
			return fmt.Errorf("expunge: %w", err)
		}
	}
	return nil
}

func uidsToSet(uids []uint32) goimap.UIDSet {
	var set goimap.UIDSet
	for _, uid := range uids {
		set = append(set, goimap.UIDRange{Start: goimap.UID(uid), Stop: goimap.UID(uid)})
	}
	return set
}

func (r *realClient) Close() error {
	return r.c.Close()
}

// Ensure io.ReadWriteCloser constraint if needed.
var _ io.Closer = (*realClient)(nil)
