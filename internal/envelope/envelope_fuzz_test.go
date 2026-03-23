package envelope

import (
	"bytes"
	"testing"
)

func FuzzDecode(f *testing.F) {
	// Seed: valid envelope
	valid, _, _ := Encode(Params{
		From:   "a@b.com",
		To:     "c@d.com",
		Packet: []byte("hello"),
	})
	f.Add(valid)

	// Seed: corrupt with transport marker
	f.Add([]byte("X-RNS-Transport: 1\r\n\r\n\x00corrupt"))

	// Seed: non-transport email
	f.Add([]byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: Hello\r\nContent-Type: text/plain\r\n\r\nHello"))

	// Seed: empty
	f.Add([]byte{})

	// Seed: oversized body
	big := append([]byte("From: a@b.com\r\nTo: c@d.com\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\nX-RNS-Transport: 1\r\n\r\n"), bytes.Repeat([]byte("AAAA"), 300000)...)
	f.Add(big)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic.
		_, _ = Decode(data)
	})
}

func FuzzRoundtrip(f *testing.F) {
	f.Add([]byte("hello"))
	f.Add([]byte{0x00, 0x01, 0x02})
	f.Add(bytes.Repeat([]byte("P"), 1000))

	f.Fuzz(func(t *testing.T, packet []byte) {
		if len(packet) == 0 || len(packet) > MaxPacketSize {
			t.Skip()
		}
		raw, _, err := Encode(Params{
			From:   "a@b.com",
			To:     "c@d.com",
			Packet: packet,
		})
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(raw)
		if err != nil {
			t.Fatalf("roundtrip decode: %v", err)
		}
		if !bytes.Equal(decoded.Packet, packet) {
			t.Errorf("roundtrip mismatch: got %d bytes, want %d bytes", len(decoded.Packet), len(packet))
		}
	})
}
