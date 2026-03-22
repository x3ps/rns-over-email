package envelope

import (
	"bytes"
	"errors"
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

func TestEncodeProducesTransportHeader(t *testing.T) {
	raw, _, err := Encode(Params{
		From:   "a@b.com",
		To:     "c@d.com",
		Packet: []byte("test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "X-RNS-Transport: 1") {
		t.Error("encoded email missing X-RNS-Transport: 1 header")
	}
}

func TestDecodeReturnsFromAndTo(t *testing.T) {
	raw, _, err := Encode(Params{
		From:   "sender@example.com",
		To:     "receiver@example.com",
		Packet: []byte("test-pkt"),
	})
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.From != "sender@example.com" {
		t.Errorf("From = %q, want sender@example.com", decoded.From)
	}
	if decoded.To != "receiver@example.com" {
		t.Errorf("To = %q, want receiver@example.com", decoded.To)
	}
}

func TestDecodeFromWithDisplayName(t *testing.T) {
	raw := strings.Join([]string{
		"From: Peer Name <peer@example.com>",
		"To: Local <local@example.com>",
		"Subject: RNS Transport Packet",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"X-RNS-Transport: 1",
		"",
		"raw binary data",
	}, "\r\n")

	decoded, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.From != "peer@example.com" {
		t.Errorf("From = %q, want peer@example.com (bare address)", decoded.From)
	}
	if decoded.To != "local@example.com" {
		t.Errorf("To = %q, want local@example.com (bare address)", decoded.To)
	}
}

func TestErrNotTransportForTextPlain(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Content-Type: text/plain",
		"",
		"just a regular email",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if !errors.Is(err, ErrNotTransport) {
		t.Errorf("expected ErrNotTransport for text/plain, got: %v", err)
	}
}

func TestErrNotTransportForTextHTML(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Content-Type: text/html",
		"",
		"<p>nope</p>",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if !errors.Is(err, ErrNotTransport) {
		t.Errorf("expected ErrNotTransport for text/html, got: %v", err)
	}
}

func TestErrNotTransportForOctetStreamWithoutMarker(t *testing.T) {
	// application/octet-stream but no X-RNS-Transport header and wrong Subject.
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Subject: Random Attachment",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"",
		"some binary data",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if !errors.Is(err, ErrNotTransport) {
		t.Errorf("expected ErrNotTransport for octet-stream without transport marker, got: %v", err)
	}
}

func TestErrNotTransportForCorrectSubjectWrongCT(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Subject: RNS Transport Packet",
		"Content-Type: text/plain",
		"",
		"has the right subject but wrong content-type",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if !errors.Is(err, ErrNotTransport) {
		t.Errorf("expected ErrNotTransport for correct subject + wrong CT, got: %v", err)
	}
}

func TestAcceptsLegacyEnvelope(t *testing.T) {
	// Legacy format: Subject + CT match, no X-RNS-Transport header.
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Subject: RNS Transport Packet",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"",
		"raw binary data",
	}, "\r\n")

	decoded, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("legacy envelope decode failed: %v", err)
	}
	if string(decoded.Packet) != "raw binary data" {
		t.Errorf("packet = %q, want %q", decoded.Packet, "raw binary data")
	}
}

func TestAcceptsNewFormatEnvelope(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Subject: anything",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"X-RNS-Transport: 1",
		"",
		"raw binary data",
	}, "\r\n")

	decoded, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("new format envelope decode failed: %v", err)
	}
	if string(decoded.Packet) != "raw binary data" {
		t.Errorf("packet = %q, want %q", decoded.Packet, "raw binary data")
	}
}

func TestCorruptBase64IsRegularError(t *testing.T) {
	// Transport envelope with corrupt base64 should be a regular error,
	// NOT ErrNotTransport (it's ours-but-broken).
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"Content-Transfer-Encoding: base64",
		"X-RNS-Transport: 1",
		"",
		"!!!not-valid-base64!!!",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if err == nil {
		t.Error("expected error for corrupt base64")
	}
	if errors.Is(err, ErrNotTransport) {
		t.Error("corrupt base64 on transport envelope should NOT be ErrNotTransport")
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
	if !errors.Is(err, ErrNotTransport) {
		t.Errorf("missing Content-Type should be ErrNotTransport, got: %v", err)
	}
}

func TestDecodeNoXRNSHeaders(t *testing.T) {
	// Legacy format with base64 encoding.
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Subject: RNS Transport Packet",
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
		"Subject: RNS Transport Packet",
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
		"Subject: RNS Transport Packet",
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
	if !strings.Contains(s, "X-RNS-Transport: 1") {
		t.Error("missing X-RNS-Transport: 1 header")
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
	header := "From: a@b.com\r\nTo: b@c.com\r\nSubject: RNS Transport Packet\r\nMIME-Version: 1.0\r\nContent-Type: application/octet-stream\r\n\r\n"
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
	header := "From: a@b.com\r\nTo: b@c.com\r\nSubject: RNS Transport Packet\r\nMIME-Version: 1.0\r\nContent-Type: application/octet-stream\r\n\r\n"
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

func TestTransportEnvelopeBrokenFromHeader(t *testing.T) {
	// Transport signals present but From header is unparseable →
	// regular error (ours-but-broken), not ErrNotTransport.
	raw := strings.Join([]string{
		"From: not a valid address!!!",
		"To: b@c.com",
		"Subject: RNS Transport Packet",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"X-RNS-Transport: 1",
		"",
		"data",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if err == nil {
		t.Fatal("expected error for broken From header")
	}
	if errors.Is(err, ErrNotTransport) {
		t.Error("broken From on transport envelope should NOT be ErrNotTransport")
	}
}

func TestTransportEnvelopeBrokenToHeader(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: not a valid address!!!",
		"Subject: RNS Transport Packet",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"X-RNS-Transport: 1",
		"",
		"data",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if err == nil {
		t.Fatal("expected error for broken To header")
	}
	if errors.Is(err, ErrNotTransport) {
		t.Error("broken To on transport envelope should NOT be ErrNotTransport")
	}
}

func TestTransportEnvelopeMissingFromHeader(t *testing.T) {
	raw := strings.Join([]string{
		"To: b@c.com",
		"Subject: RNS Transport Packet",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"X-RNS-Transport: 1",
		"",
		"data",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if err == nil {
		t.Fatal("expected error for missing From header")
	}
	if errors.Is(err, ErrNotTransport) {
		t.Error("missing From on transport envelope should NOT be ErrNotTransport")
	}
}

// H1 tests: unparseable messages with/without transport markers.

func TestUnparseableMessageWithoutMarkerIsNotTransport(t *testing.T) {
	raw := []byte("this is garbage, not an email at all")
	_, err := Decode(raw)
	if !errors.Is(err, ErrNotTransport) {
		t.Errorf("expected ErrNotTransport for garbage without marker, got: %v", err)
	}
}

func TestUnparseableMessageWithMarkerIsOursBroken(t *testing.T) {
	// Garbled body but raw X-RNS-Transport: 1 in headers → regular error.
	raw := []byte("X-RNS-Transport: 1\r\n\r\n\x00\x00corrupt body")
	_, err := Decode(raw)
	if err == nil {
		t.Fatal("expected error for corrupt transport message")
	}
	if errors.Is(err, ErrNotTransport) {
		t.Error("corrupt message with transport marker should NOT be ErrNotTransport")
	}
}

func TestLegacyMarkerDetectedInRawScan(t *testing.T) {
	// Corrupted legacy message with Subject: RNS Transport Packet → ours-but-broken.
	// Malformed header line causes mail.ReadMessage() to fail, triggering raw scan.
	raw := []byte("Subject: RNS Transport Packet\r\nBadHeaderNoColon\r\n\r\ncorrupt")
	_, err := Decode(raw)
	if err == nil {
		t.Fatal("expected error for corrupt legacy transport message")
	}
	if errors.Is(err, ErrNotTransport) {
		t.Error("corrupt message with legacy subject should NOT be ErrNotTransport")
	}
}

func TestHasTransportMarker(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want bool
	}{
		{"new format", []byte("X-RNS-Transport: 1\r\nFrom: a@b.com\r\n\r\nbody"), true},
		{"legacy format", []byte("Subject: RNS Transport Packet\r\n\r\nbody"), true},
		{"no marker", []byte("Subject: Hello\r\n\r\nbody"), false},
		{"marker in body (not headers)", []byte("Subject: Hello\r\n\r\nX-RNS-Transport: 1"), false},
		{"case insensitive header name", []byte("x-rns-transport: 1\r\n\r\nbody"), true},
		{"wrong value", []byte("X-RNS-Transport: 2\r\n\r\nbody"), false},
		{"empty input", []byte(""), false},
		{"LF only line endings", []byte("X-RNS-Transport: 1\n\nbody"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasTransportMarker(tt.raw); got != tt.want {
				t.Errorf("hasTransportMarker = %v, want %v", got, tt.want)
			}
		})
	}
}

// H2 tests: transport header with missing/broken/wrong Content-Type.

func TestTransportHeaderWithMissingContentType(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"X-RNS-Transport: 1",
		"",
		"data",
	}, "\r\n")
	_, err := Decode([]byte(raw))
	if err == nil {
		t.Fatal("expected error for missing Content-Type with transport header")
	}
	if errors.Is(err, ErrNotTransport) {
		t.Error("transport header + missing CT should NOT be ErrNotTransport")
	}
}

func TestTransportHeaderWithBrokenContentType(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Content-Type: ;;;invalid;;;",
		"X-RNS-Transport: 1",
		"",
		"data",
	}, "\r\n")
	_, err := Decode([]byte(raw))
	if err == nil {
		t.Fatal("expected error for broken Content-Type with transport header")
	}
	if errors.Is(err, ErrNotTransport) {
		t.Error("transport header + broken CT should NOT be ErrNotTransport")
	}
}

func TestTransportHeaderWithWrongContentType(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Content-Type: text/plain",
		"X-RNS-Transport: 1",
		"",
		"data",
	}, "\r\n")
	_, err := Decode([]byte(raw))
	if err == nil {
		t.Fatal("expected error for wrong Content-Type with transport header")
	}
	if errors.Is(err, ErrNotTransport) {
		t.Error("transport header + wrong CT should NOT be ErrNotTransport")
	}
}

// H4 tests: Content-Transfer-Encoding handling.

func TestDecodeBodyQuotedPrintable(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Subject: RNS Transport Packet",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"Content-Transfer-Encoding: quoted-printable",
		"X-RNS-Transport: 1",
		"",
		"hello=20world",
	}, "\r\n")

	decoded, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if string(decoded.Packet) != "hello world" {
		t.Errorf("packet = %q, want %q", decoded.Packet, "hello world")
	}
}

func TestDecodeBodyQuotedPrintableEmpty(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Subject: RNS Transport Packet",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"Content-Transfer-Encoding: quoted-printable",
		"X-RNS-Transport: 1",
		"",
		"",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if err == nil {
		t.Error("expected error for empty QP body, got nil")
	}
}

func TestDecodeBodyUnknownEncoding(t *testing.T) {
	raw := strings.Join([]string{
		"From: a@b.com",
		"To: b@c.com",
		"Subject: RNS Transport Packet",
		"MIME-Version: 1.0",
		"Content-Type: application/octet-stream",
		"Content-Transfer-Encoding: x-custom-encoding",
		"X-RNS-Transport: 1",
		"",
		"data",
	}, "\r\n")

	_, err := Decode([]byte(raw))
	if err == nil {
		t.Error("expected error for unknown CTE, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported Content-Transfer-Encoding") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDecodeBodyIdentityEncodings(t *testing.T) {
	for _, cte := range []string{"7bit", "8bit", "binary", ""} {
		t.Run("cte="+cte, func(t *testing.T) {
			headers := []string{
				"From: a@b.com",
				"To: b@c.com",
				"Subject: RNS Transport Packet",
				"MIME-Version: 1.0",
				"Content-Type: application/octet-stream",
				"X-RNS-Transport: 1",
			}
			if cte != "" {
				headers = append(headers, "Content-Transfer-Encoding: "+cte)
			}
			headers = append(headers, "", "raw binary data")
			raw := strings.Join(headers, "\r\n")

			decoded, err := Decode([]byte(raw))
			if err != nil {
				t.Fatalf("decode failed for CTE=%q: %v", cte, err)
			}
			if string(decoded.Packet) != "raw binary data" {
				t.Errorf("packet = %q, want %q", decoded.Packet, "raw binary data")
			}
		})
	}
}
