package dnsmsg

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// digQueryHex is a real query packet produced by dig 9.10.6:
// `dig www.example.com A` — verified against RFC 1035 §4 by hand and by
// round-tripping through this package.
const digQueryHex = "26e60120000100000000000103777777076578616d706c6503636f6d00000100010000291000000000000000"

// TestDigInteropQuery parses a packet captured from dig and re-encodes it.
func TestDigInteropQuery(t *testing.T) {
	raw, err := hex.DecodeString(digQueryHex)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Unpack(raw)
	if err != nil {
		t.Fatalf("cannot parse dig's packet: %v", err)
	}
	if m.ID != 0x26e6 || !m.RD || m.QR {
		t.Fatalf("header: %+v", m.Header)
	}
	if len(m.Questions) != 1 || m.Questions[0].Name != "www.example.com." ||
		m.Questions[0].Type != TypeA || m.Questions[0].Class != ClassIN {
		t.Fatalf("question: %+v", m.Questions)
	}
	if !m.HasEDNS() || m.UDPSize() != 4096 {
		t.Fatalf("edns: size=%d", m.UDPSize())
	}
	// re-pack: everything but the flags field (dig sets AD, which we do
	// not model) must be byte-identical
	out, err := m.Pack(0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out[:2], raw[:2]) || !bytes.Equal(out[4:], raw[4:]) {
		t.Fatalf("re-pack differs:\ngot  %x\nwant %x", out, raw)
	}
}

// TestGoldenQueryBytes pins our encoder to the RFC 1035 wire layout for a
// plain query without EDNS.
func TestGoldenQueryBytes(t *testing.T) {
	m := &Message{
		Header:    Header{ID: 0x1234, RD: true},
		Questions: []Question{{Name: "www.example.com.", Type: TypeA, Class: ClassIN}},
	}
	got, err := m.Pack(0)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString("12340100000100000000000003777777076578616d706c6503636f6d0000010001")
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}
}

// TestGoldenAnswerBytes pins the answer layout including a compression
// pointer back to the question name (RFC 1035 §4.1.4).
func TestGoldenAnswerBytes(t *testing.T) {
	m := &Message{
		Header:    Header{ID: 0x1234, QR: true, AA: true, RD: true},
		Questions: []Question{{Name: "www.example.com.", Type: TypeA, Class: ClassIN}},
		Answers: []RR{{
			Name: "www.example.com.", Type: TypeA, Class: ClassIN, TTL: 300,
			Data: &A{IP: []byte{192, 0, 2, 1}},
		}},
	}
	got, err := m.Pack(0)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString(
		"123485000001000100000000" + // header: QR AA RD, 1 question 1 answer
			"03777777076578616d706c6503636f6d0000010001" + // question
			"c00c" + // owner: pointer to offset 12
			"0001" + "0001" + "0000012c" + "0004" + "c0000201") // A 192.0.2.1
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}
}
