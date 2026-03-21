package envelope

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/google/uuid"
)

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
	h.Set("Subject", "RNS Transport Packet")
	h.Set("Date", time.Now().UTC().Format(time.RFC1123Z))
	h.Set("Message-ID", mid)
	h.Set("MIME-Version", "1.0")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Transfer-Encoding", "base64")

	// Write headers using canonical format.
	for _, key := range []string{"From", "To", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type", "Content-Transfer-Encoding"} {
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

// maxBodySize limits how many bytes Decode will read from the message body
// to prevent OOM from maliciously large emails (1 MB).
const maxBodySize = 1 << 20

// Decoded holds the parsed result of a MIME envelope.
type Decoded struct {
	MessageID string
	Packet    []byte
}

// Decode parses a MIME email and extracts the RNS packet.
func Decode(raw []byte) (*Decoded, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}

	d := &Decoded{
		MessageID: msg.Header.Get("Message-ID"),
	}

	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		return nil, fmt.Errorf("missing Content-Type header")
	}

	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, fmt.Errorf("parse content-type: %w", err)
	}

	if mediaType != "application/octet-stream" {
		return nil, fmt.Errorf("unsupported content-type: %s", mediaType)
	}

	body, err := io.ReadAll(io.LimitReader(msg.Body, maxBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxBodySize {
		return nil, fmt.Errorf("body exceeds maximum size of %d bytes", maxBodySize)
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
	if strings.EqualFold(cte, "base64") {
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
	}
	return body, nil
}
