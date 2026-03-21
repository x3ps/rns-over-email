package transport

import "context"

// Sender sends an encoded email message to a recipient.
type Sender interface {
	Send(ctx context.Context, to string, msg []byte) error
	Probe(ctx context.Context) error // dial + auth + quit, no message sent
}
