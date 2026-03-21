package envelope

import (
	"bytes"
	"strings"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	packet := []byte("hello RNS world, this is a test packet payload")
	raw, messageID, err := Encode(Params{
		From:   "sender@test.com",
		To:     "receiver@test.com",
		Packet: packet,
	})
	if err != nil {
		t.Fatal(err)
	}
	if messageID == "" {
		t.Error("message ID is empty")
	}

	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}

	if decoded.MessageID != messageID {
		t.Errorf("message ID = %q, want %q", decoded.MessageID, messageID)
	}
	if !bytes.Equal(decoded.Packet, packet) {
		t.Errorf("packet mismatch:\n  got:  %x\n  want: %x", decoded.Packet, packet)
	}
}

func TestDecodeSinglePartNoBase64(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"",
		"raw binary data",
	}, "\r\n")

	decoded, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("single-part decode failed: %v", err)
	}
	if string(decoded.Packet) != "raw binary data" {
		t.Errorf("packet = %q, want %q", decoded.Packet, "raw binary data")
	}
}

func TestDecodeUnsupportedContentType(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Content-Type: text/html",
		"",
		"<p>nope</p>",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if err == nil {
		t.Error("expected error for unsupported content-type")
	}
}

func TestEncodeSinglePartFormat(t *testing.T) {
	raw, _, err := Encode(Params{
		From:   "a@b.com",
		To:     "c@d.com",
		Packet: []byte("test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Contains(s, "multipart") {
		t.Error("new format should not use multipart")
	}
	if !strings.Contains(s, "Content-Type: application/octet-stream") {
		t.Error("missing Content-Type: application/octet-stream header")
	}
	if !strings.Contains(s, "Content-Transfer-Encoding: base64") {
		t.Error("missing Content-Transfer-Encoding: base64 header")
	}
	if strings.Contains(s, "X-RNS") {
		t.Error("should not include any X-RNS headers")
	}
}

func TestPacketRoundtrip(t *testing.T) {
	large := make([]byte, 10000)
	for i := range large {
		large[i] = byte(i % 256)
	}

	tests := []struct {
		name string
		pkt  []byte
	}{
		{"single byte", []byte{0x01}},
		{"single zero byte", []byte{0x00}},
		{"all zeros 64B", make([]byte, 64)},
		{"all 0xFF 64B", bytes.Repeat([]byte{0xFF}, 64)},
		{"near MTU 500B", make([]byte, 500)},
		{"large 10KB", large},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, _, err := Encode(Params{
				From:   "a@b.com",
				To:     "c@d.com",
				Packet: tt.pkt,
			})
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decoded.Packet, tt.pkt) {
				t.Errorf("roundtrip mismatch: got %d bytes, want %d", len(decoded.Packet), len(tt.pkt))
			}
		})
	}
}

func TestDecodeNoXRNSHeaders(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"Content-Transfer-Encoding: base64",
		"",
		"aGVsbG8gd29ybGQ=",
	}, "\r\n")

	decoded, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("decode without X-RNS headers failed: %v", err)
	}
	if string(decoded.Packet) != "hello world" {
		t.Errorf("packet = %q, want %q", decoded.Packet, "hello world")
	}
}

func TestCorruptBase64SinglePart(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"Content-Transfer-Encoding: base64",
		"",
		"!!!not-valid-base64!!!",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if err == nil {
		t.Error("expected error for corrupt base64")
	}
}

func TestDecodeEmptyBase64Body(t *testing.T) {
	// base64("") == "" — decodeBody returns []byte{}, not nil.
	// len check must catch this and return an error.
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"Content-Transfer-Encoding: base64",
		"",
		"",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if err == nil {
		t.Error("expected error for empty base64 body, got nil")
	}
}

func TestEncodeRequiresParams(t *testing.T) {
	tests := []struct {
		name string
		p    Params
	}{
		{"empty from", Params{From: "", To: "b@c.com", Packet: []byte("x")}},
		{"empty to", Params{From: "a@b.com", To: "", Packet: []byte("x")}},
		{"nil packet", Params{From: "a@b.com", To: "b@c.com", Packet: nil}},
		{"empty packet", Params{From: "a@b.com", To: "b@c.com", Packet: []byte{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Encode(tt.p)
			if err == nil {
				t.Error("expected error for missing params, got nil")
			}
		})
	}
}

func TestDecodeBodyTooLarge(t *testing.T) {
	// Build a message whose body exceeds maxBodySize.
	header := "From: a@b.com\r\nTo: b@c.com\r\nMIME-Version: 1.0\r\nContent-Type: application/octet-stream\r\n\r\n"
	body := make([]byte, maxBodySize+1)
	for i := range body {
		body[i] = 'A'
	}
	raw := append([]byte(header), body...)

	_, err := Decode(raw)
	if err == nil {
		t.Error("expected error for oversized body, got nil")
	}
}

func TestDomainFromAddr(t *testing.T) {
	if d := domainFromAddr("user@example.com"); d != "example.com" {
		t.Errorf("domainFromAddr = %q, want example.com", d)
	}
	if d := domainFromAddr("noat"); d != "rns-transport" {
		t.Errorf("domainFromAddr = %q, want rns-transport", d)
	}
}

func TestDecodeMissingContentType(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"MIME-Version: 1.0",
		"",
		"some body",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if err == nil {
		t.Error("expected error for missing Content-Type, got nil")
	}
	if !strings.Contains(err.Error(), "missing Content-Type") {
		t.Errorf("error = %q, want 'missing Content-Type'", err.Error())
	}
}

func TestDecodeMultipleBase64Lines(t *testing.T) {
	// Create a packet large enough to produce multi-line base64.
	packet := bytes.Repeat([]byte("A"), 100)
	raw, _, err := Encode(Params{
		From:   "a@b.com",
		To:     "c@d.com",
		Packet: packet,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify the encoded output contains multiple base64 lines.
	body := string(raw[bytes.Index(raw, []byte("\r\n\r\n"))+4:])
	lines := strings.Split(strings.TrimRight(body, "\r\n"), "\r\n")
	if len(lines) < 2 {
		t.Fatalf("expected multiple base64 lines, got %d", len(lines))
	}

	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Packet, packet) {
		t.Errorf("decoded packet mismatch: got %d bytes, want %d", len(decoded.Packet), len(packet))
	}
}

func TestMessageIDFormat(t *testing.T) {
	_, messageID, err := Encode(Params{
		From:   "sender@example.com",
		To:     "receiver@example.com",
		Packet: []byte("msg-id-test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(messageID, "<") || !strings.HasSuffix(messageID, ">") {
		t.Errorf("messageID = %q, want <uuid@domain> format", messageID)
	}
	inner := messageID[1 : len(messageID)-1]
	atIdx := strings.LastIndex(inner, "@")
	if atIdx < 0 {
		t.Errorf("messageID %q contains no @", messageID)
		return
	}
	if domain := inner[atIdx+1:]; domain != "example.com" {
		t.Errorf("messageID domain = %q, want example.com", domain)
	}
}

func TestBodySizeExactlyAtLimit(t *testing.T) {
	// Body of exactly maxBodySize bytes must be accepted (boundary: > not >=).
	header := "From: a@b.com\r\nTo: b@c.com\r\nMIME-Version: 1.0\r\nContent-Type: application/octet-stream\r\n\r\n"
	body := bytes.Repeat([]byte("A"), maxBodySize)
	raw := append([]byte(header), body...)

	_, err := Decode(raw)
	if err != nil {
		t.Errorf("expected success for body exactly at limit, got: %v", err)
	}
}

func TestEncodeBase64LineWrapping(t *testing.T) {
	// 100 bytes of input produces >76 chars of base64, requiring wrapping.
	packet := bytes.Repeat([]byte("X"), 100)
	raw, _, err := Encode(Params{
		From:   "a@b.com",
		To:     "c@d.com",
		Packet: packet,
	})
	if err != nil {
		t.Fatal(err)
	}

	body := string(raw[bytes.Index(raw, []byte("\r\n\r\n"))+4:])
	for _, line := range strings.Split(strings.TrimRight(body, "\r\n"), "\r\n") {
		if len(line) > 76 {
			t.Errorf("base64 line exceeds 76 chars (RFC 2045): len=%d, line=%q", len(line), line)
		}
	}
}
