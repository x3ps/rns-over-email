package envelope

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrNotTransport is returned by Decode when the message is not an RNS
// transport envelope (e.g. a regular email). Callers can check with
// errors.Is(err, ErrNotTransport) to distinguish "not ours" from "ours but
// broken".
var ErrNotTransport = errors.New("not an RNS transport message")

// transportHeader is the header added to new-format outbound envelopes.
const transportHeader = "X-RNS-Transport"

// legacySubject is the Subject value used by the legacy envelope format.
const legacySubject = "RNS Transport Packet"

// Params holds the metadata needed to construct an email envelope.
type Params struct {
	From   string
	To     string
	Packet []byte
}

// Encode creates a single-part application/octet-stream email with the RNS packet.
// Returns the raw email bytes, the generated Message-ID, and any error.
func Encode(p Params) ([]byte, string, error) {
	if p.From == "" || p.To == "" || len(p.Packet) == 0 {
		return nil, "", fmt.Errorf("encode: From, To and Packet are required")
	}

	mid := "<" + uuid.New().String() + "@" + domainFromAddr(p.From) + ">"

	var buf bytes.Buffer
	h := textproto.MIMEHeader{}
	h.Set("From", p.From)
	h.Set("To", p.To)
	h.Set("Subject", legacySubject)
	h.Set("Date", time.Now().UTC().Format(time.RFC1123Z))
	h.Set("Message-ID", mid)
	h.Set("MIME-Version", "1.0")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Transfer-Encoding", "base64")
	h.Set(transportHeader, "1")

	// Write headers using canonical format.
	for _, key := range []string{
		"From", "To", "Subject", "Date", "Message-ID",
		"MIME-Version", "Content-Type", "Content-Transfer-Encoding",
		transportHeader,
	} {
		fmt.Fprintf(&buf, "%s: %s\r\n", key, h.Get(key))
	}
	buf.WriteString("\r\n")

	// Base64 body wrapped at 76 chars per RFC 2045.
	encoded := base64.StdEncoding.EncodeToString(p.Packet)
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		buf.WriteString(encoded[i:end])
		buf.WriteString("\r\n")
	}

	return buf.Bytes(), mid, nil
}

// domainFromAddr extracts the domain from an email address.
func domainFromAddr(addr string) string {
	// Strip display name wrapper if present (e.g. "User <user@example.com>").
	if parsed, err := mail.ParseAddress(addr); err == nil {
		addr = parsed.Address
	}
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return "rns-transport"
}

// MaxBodySize limits how many bytes Decode will read from the message body
// to prevent OOM from maliciously large emails (1 MB).
const MaxBodySize = 1 << 20

// MaxPacketSize is the largest packet that roundtrips through Encode → Decode
// without the base64-wrapped body exceeding MaxBodySize.
//
// Derivation: Encode wraps base64 at 76 chars + "\r\n" per line (78 bytes
// encoding 57 raw bytes). For MaxBodySize bytes of body budget:
//
//	full lines:  floor(MaxBodySize / 78) * 57  decoded bytes
//	remainder:   (MaxBodySize%78 - 2) / 4 * 3  decoded bytes  (subtract \r\n, base64-decode)
const MaxPacketSize = (MaxBodySize/78)*57 + (MaxBodySize%78-2)/4*3 // 766266

// Decoded holds the parsed result of a MIME envelope.
type Decoded struct {
	MessageID string
	From      string // bare email address (no display name)
	To        string // bare email address (no display name)
	Packet    []byte
}

// isTransport determines whether a parsed message is an RNS transport envelope.
// Returns true if the message matches either the new format (X-RNS-Transport
// header) or the legacy format (Subject + Content-Type match).
func isTransport(header mail.Header, mediaType string) bool {
	// New format: X-RNS-Transport: 1
	if header.Get(transportHeader) == "1" && mediaType == "application/octet-stream" {
		return true
	}
	// Legacy format: Subject + Content-Type
	if header.Get("Subject") == legacySubject && mediaType == "application/octet-stream" {
		return true
	}
	return false
}

// extractAddress parses an email address header and returns the bare address.
// Returns an error if the header is missing or unparseable.
func extractAddress(header mail.Header, key string) (string, error) {
	raw := header.Get(key)
	if raw == "" {
		return "", fmt.Errorf("missing %s header", key)
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil {
		return "", fmt.Errorf("parse %s address: %w", key, err)
	}
	return addr.Address, nil
}

// Decode parses a MIME email and extracts the RNS packet.
//
// Two-phase processing:
//   - Phase A (headers only): classify transport vs non-transport, extract
//     From/To. Returns ErrNotTransport early for non-transport messages
//     (before reading body).
//   - Phase B (body): read and decode body for confirmed transport messages.
func Decode(raw []byte) (*Decoded, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		// H1: Parse failed. Raw-scan for transport marker to distinguish
		// "not ours" (safe to skip) from "ours but corrupted" (preserve).
		if hasTransportMarker(raw) {
			return nil, fmt.Errorf("parse transport message: %w", err)
		}
		return nil, fmt.Errorf("%w: parse message: %v", ErrNotTransport, err)
	}

	// Phase A: classify by headers.
	// H2: Check transport header BEFORE Content-Type so that a transport
	// message with a broken/missing Content-Type is "ours but broken"
	// (regular error) rather than "not ours" (ErrNotTransport).
	hasMarker := msg.Header.Get(transportHeader) == "1"

	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		if hasMarker {
			return nil, fmt.Errorf("transport envelope: missing Content-Type header")
		}
		return nil, fmt.Errorf("%w: missing Content-Type header", ErrNotTransport)
	}

	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		if hasMarker {
			return nil, fmt.Errorf("transport envelope: parse content-type: %v", err)
		}
		return nil, fmt.Errorf("%w: parse content-type: %v", ErrNotTransport, err)
	}

	if !isTransport(msg.Header, mediaType) {
		if hasMarker {
			return nil, fmt.Errorf("transport envelope: unexpected content-type=%s", mediaType)
		}
		return nil, fmt.Errorf("%w: content-type=%s", ErrNotTransport, mediaType)
	}

	// Message matches transport signals — extract From/To.
	// If From/To headers are broken on a transport message, this is
	// ours-but-broken (regular error), NOT ErrNotTransport.
	from, err := extractAddress(msg.Header, "From")
	if err != nil {
		return nil, fmt.Errorf("transport envelope: %w", err)
	}
	to, err := extractAddress(msg.Header, "To")
	if err != nil {
		return nil, fmt.Errorf("transport envelope: %w", err)
	}

	d := &Decoded{
		MessageID: msg.Header.Get("Message-ID"),
		From:      from,
		To:        to,
	}

	// Phase B: read and decode body.
	body, err := io.ReadAll(io.LimitReader(msg.Body, MaxBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > MaxBodySize {
		return nil, fmt.Errorf("body exceeds maximum size of %d bytes", MaxBodySize)
	}
	d.Packet, err = decodeBody(body, msg.Header.Get("Content-Transfer-Encoding"))
	if err != nil {
		return nil, err
	}

	if len(d.Packet) == 0 {
		return nil, fmt.Errorf("no packet found")
	}

	return d, nil
}

func decodeBody(body []byte, cte string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		cleaned := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' {
				return -1
			}
			return r
		}, string(body))
		decoded, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return nil, fmt.Errorf("decode base64: %w", err)
		}
		return decoded, nil
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return nil, fmt.Errorf("decode quoted-printable: %w", err)
		}
		return decoded, nil
	case "7bit", "8bit", "binary", "":
		return body, nil
	default:
		return nil, fmt.Errorf("unsupported Content-Transfer-Encoding: %q", cte)
	}
}

// hasTransportMarker scans raw email bytes for transport signals
// without requiring a full RFC 5322 parse. Used as a fallback
// when mail.ReadMessage() fails on a corrupted message.
// Detects both new format (X-RNS-Transport: 1) and legacy format
// (Subject: RNS Transport Packet).
func hasTransportMarker(raw []byte) bool {
	headerEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		headerEnd = bytes.Index(raw, []byte("\n\n"))
	}
	if headerEnd < 0 {
		headerEnd = len(raw)
	}
	for _, line := range bytes.Split(raw[:headerEnd], []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 || line[0] == ' ' || line[0] == '\t' {
			continue // skip folded continuation lines
		}
		parts := bytes.SplitN(line, []byte(":"), 2)
		if len(parts) != 2 {
			continue
		}
		name := bytes.TrimSpace(parts[0])
		value := bytes.TrimSpace(parts[1])
		if bytes.EqualFold(name, []byte("X-RNS-Transport")) && string(value) == "1" {
			return true
		}
		if bytes.EqualFold(name, []byte("Subject")) && string(value) == legacySubject {
			return true
		}
	}
	return false
}
