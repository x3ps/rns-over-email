package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/x3ps/rns-iface-email/internal/envelope"
	"github.com/x3ps/rns-iface-email/internal/transport"
)

const (
	imapDialTimeout = 30 * time.Second
	imapIOTimeout   = 60 * time.Second
	imapIdleTimeout = 30 * time.Minute

	// maxHeaderSize is the budget for RFC 5322 headers in the fetch
	// literal reader. Both peers use our Encode(), which produces ~400
	// bytes of headers with transport markers placed early. 16 KiB
	// accommodates all realistic cases including MTA-added headers.
	//
	// For messages with pathologically long headers (>16 KiB),
	// mail.ReadMessage() on truncated input may fail, but
	// hasTransportMarker() fallback scans the available bytes for
	// X-RNS-Transport/Subject markers — correct classification.
	maxHeaderSize = 16 << 10

	// maxFetchLiteralSize caps bytes buffered per message in FetchSince.
	// Composed from explicit header and body budgets so that Decode()
	// can always run its full two-phase classification on the input.
	// The +1 ensures Decode() can detect body-exceeds-limit.
	maxFetchLiteralSize = maxHeaderSize + envelope.MaxBodySize + 1
)

// errNoUIDPlus is returned by DeleteUIDs when the server lacks the UIDPLUS
// capability (RFC 4315). Without UIDPLUS, plain EXPUNGE removes ALL \Deleted
// messages in the mailbox (RFC 3501 §6.4.3), not just ours. To avoid setting
// \Deleted flags without a safe way to expunge them, the entire delete
// operation is refused before any mailbox mutation occurs.
var errNoUIDPlus = errors.New("delete cleanup requires UIDPLUS capability (RFC 4315)")

// errUnsafeMove is returned by MoveUIDs when the server lacks both MOVE
// (RFC 6851) and UIDPLUS (RFC 4315). Without MOVE, the client library falls
// back to COPY+STORE+EXPUNGE. Without UIDPLUS, the plain EXPUNGE removes ALL
// \Deleted messages in the mailbox, not just ours.
var errUnsafeMove = errors.New("move cleanup unsafe: server lacks both MOVE (RFC 6851) and UIDPLUS (RFC 4315); plain EXPUNGE would remove all \\Deleted messages")

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
// If tlsCfg is nil, a default config with ServerName=host is used for TLS modes.
func Dial(ctx context.Context, host string, port int, tlsMode string, onMailbox func(), tlsCfg *tls.Config) (*realClient, error) {
	opts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil && onMailbox != nil {
					onMailbox()
				}
			},
		},
	}

	tc, dialErr := transport.DialTCP(ctx, host, port, tlsMode, imapDialTimeout, imapIOTimeout, tlsCfg)
	if dialErr != nil {
		if tlsMode == "tls" {
			return nil, fmt.Errorf("imap dial tls: %w", dialErr)
		}
		return nil, fmt.Errorf("imap dial: %w", dialErr)
	}

	var c *imapclient.Client
	if tlsMode == "starttls" {
		var err error
		if tlsCfg != nil {
			opts.TLSConfig = tlsCfg
		} else {
			opts.TLSConfig = &tls.Config{ServerName: host}
		}
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

// readLiteral reads from r up to limit bytes. If the source exceeds limit,
// the remainder is drained to keep the IMAP protocol in sync and oversized
// is set to true. The returned buf is at most limit bytes.
func readLiteral(r io.Reader, limit int) (buf []byte, oversized bool, err error) {
	limited := io.LimitReader(r, int64(limit)+1)
	buf, err = io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if len(buf) > limit {
		_, _ = io.Copy(io.Discard, r) // drain excess
		buf = buf[:limit]
		return buf, true, nil
	}
	return buf, false, nil
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

		var uid goimap.UID
		var raw []byte
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			switch v := item.(type) {
			case imapclient.FetchItemDataUID:
				uid = v.UID
			case imapclient.FetchItemDataBodySection:
				if v.Literal == nil {
					continue
				}
				var err error
				raw, _, err = readLiteral(v.Literal, maxFetchLiteralSize)
				if err != nil {
					return fmt.Errorf("fetch read body: %w", err)
				}
			}
		}

		if uint32(uid) <= lastUID {
			// Server returned UID <= lastUID (can happen with UID FETCH *).
			continue
		}
		if raw == nil {
			continue
		}
		if err := handler(uint32(uid), raw); err != nil {
			return err
		}
	}
	return nil
}

// idleWait races ctx.Done() against waitCh. If waitCh fires first
// (server disconnect/BYE), returns its error without calling closeFn.
// If ctx.Done() fires first, calls closeFn then drains waitCh.
func idleWait(ctx context.Context, waitCh <-chan error, closeFn func() error) error {
	select {
	case err := <-waitCh:
		return err
	case <-ctx.Done():
		if err := closeFn(); err != nil {
			<-waitCh
			return fmt.Errorf("idle close: %w", err)
		}
		return <-waitCh
	}
}

func (r *realClient) Idle(ctx context.Context) error {
	r.tc.SetTimeout(imapIdleTimeout)
	defer r.tc.SetTimeout(imapIOTimeout)

	idleCmd, err := r.c.Idle()
	if err != nil {
		return fmt.Errorf("idle start: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- idleCmd.Wait() }()

	return idleWait(ctx, waitCh, func() error {
		r.tc.SetTimeout(imapIOTimeout)
		return idleCmd.Close()
	})
}

func (r *realClient) HasIdle() bool {
	return hasIdle(r.c.Caps())
}

func (r *realClient) Noop() error {
	return r.c.Noop().Wait()
}

func (r *realClient) MoveUIDs(uids []uint32, dest string) error {
	if len(uids) == 0 {
		return nil
	}
	if err := requireSafeMove(r.c.Caps()); err != nil {
		return err
	}
	uidSet := uidsToSet(uids)
	_, err := r.c.Move(uidSet, dest).Wait()
	if err != nil {
		return fmt.Errorf("move uids: %w", err)
	}
	return nil
}

// requireUIDPlus returns errNoUIDPlus if the server lacks UIDPLUS.
// This is the pre-mutation gate used by DeleteUIDs.
func requireUIDPlus(caps goimap.CapSet) error {
	if !hasUIDPlus(caps) {
		return errNoUIDPlus
	}
	return nil
}

func (r *realClient) DeleteUIDs(uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}

	// Check UIDPLUS capability BEFORE any mailbox mutation. Without UIDPLUS,
	// plain EXPUNGE removes ALL \Deleted messages (RFC 3501 §6.4.3), not just
	// ours. Setting \Deleted without a safe expunge path would leave hidden
	// flags that confuse other clients, so we refuse the entire operation.
	if err := requireUIDPlus(r.c.Caps()); err != nil {
		return err
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

	// UID EXPUNGE (RFC 4315) removes only the specific UIDs we flagged.
	if err := r.c.UIDExpunge(uidSet).Close(); err != nil {
		return fmt.Errorf("uid expunge: %w", err)
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

// hasUIDPlus reports whether the server supports UIDPLUS (RFC 4315).
// Uses CapSet.Has() which correctly handles IMAP4rev2 implied capabilities.
func hasUIDPlus(caps goimap.CapSet) bool { return caps.Has(goimap.CapUIDPlus) }

// hasMove reports whether the server supports MOVE (RFC 6851).
// Uses CapSet.Has() which correctly handles IMAP4rev2 implied capabilities.
func hasMove(caps goimap.CapSet) bool { return caps.Has(goimap.CapMove) }

// requireSafeMove returns errUnsafeMove if the server lacks both MOVE and
// UIDPLUS. With MOVE, the operation is atomic. With UIDPLUS (but no MOVE),
// the fallback COPY+STORE+UID EXPUNGE is safe. Without either, plain EXPUNGE
// would remove all \Deleted messages.
func requireSafeMove(caps goimap.CapSet) error {
	if !hasMove(caps) && !hasUIDPlus(caps) {
		return errUnsafeMove
	}
	return nil
}

// hasIdle reports whether the server supports IDLE (RFC 2177).
// Uses CapSet.Has() which correctly handles IMAP4rev2 implied capabilities.
func hasIdle(caps goimap.CapSet) bool { return caps.Has(goimap.CapIdle) }

// Ensure io.ReadWriteCloser constraint if needed.
var _ io.Closer = (*realClient)(nil)
