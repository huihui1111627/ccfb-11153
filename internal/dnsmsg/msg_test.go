package dnsmsg

import (
	"bytes"
	"net"
	"strings"
	"testing"
)

func sampleMessage() *Message {
	return &Message{
		Header: Header{ID: 0x1234, QR: true, AA: true, RD: true, Rcode: RcodeNoError},
		Questions: []Question{
			{Name: "www.example.com.", Qtype: TypeA, Qclass: ClassIN},
		},
		Answers: []RR{
			{Name: "www.example.com.", Type: TypeA, Class: ClassIN, TTL: 300, IP: net.ParseIP("192.0.2.1")},
			{Name: "www.example.com.", Type: TypeAAAA, Class: ClassIN, TTL: 300, IP: net.ParseIP("2001:db8::1")},
			{Name: "alias.example.com.", Type: TypeCNAME, Class: ClassIN, TTL: 60, Target: "www.example.com."},
			{Name: "example.com.", Type: TypeMX, Class: ClassIN, TTL: 600, Preference: 10, Target: "mail.example.com."},
			{Name: "example.com.", Type: TypeTXT, Class: ClassIN, TTL: 600, Text: []string{"v=spf1 -all", strings.Repeat("x", 300)}},
			{Name: "_sip._tcp.example.com.", Type: TypeSRV, Class: ClassIN, TTL: 60, Priority: 10, Weight: 5, Port: 5060, Target: "sip.example.com."},
			{Name: "example.com.", Type: TypeNS, Class: ClassIN, TTL: 3600, Target: "ns1.example.com."},
		},
		Authorities: []RR{
			{Name: "example.com.", Type: TypeSOA, Class: ClassIN, TTL: 3600,
				Target: "ns1.example.com.", Rname: "hostmaster.example.com.",
				Serial: 2024010101, Refresh: 7200, Retry: 3600, Expire: 1209600, Minimum: 300},
		},
	}
}

func TestPackUnpackRoundTrip(t *testing.T) {
	for _, compress := range []bool{true, false} {
		m := sampleMessage()
		m.EDNSSize = 1232
		buf, err := m.Pack(compress)
		if err != nil {
			t.Fatalf("pack(compress=%v): %v", compress, err)
		}
		got, err := Unpack(buf)
		if err != nil {
			t.Fatalf("unpack(compress=%v): %v", compress, err)
		}
		if got.ID != m.ID || !got.QR || !got.AA || !got.RD {
			t.Fatalf("header mismatch: %+v", got.Header)
		}
		if got.EDNSSize != 1232 {
			t.Fatalf("edns size = %d", got.EDNSSize)
		}
		if len(got.Questions) != 1 || got.Questions[0].Name != "www.example.com." {
			t.Fatalf("questions: %+v", got.Questions)
		}
		if len(got.Answers) != 7 {
			t.Fatalf("answers = %d", len(got.Answers))
		}
		if got.Answers[0].IP.String() != "192.0.2.1" {
			t.Fatalf("A = %v", got.Answers[0].IP)
		}
		if got.Answers[1].IP.String() != "2001:db8::1" {
			t.Fatalf("AAAA = %v", got.Answers[1].IP)
		}
		if got.Answers[2].Target != "www.example.com." {
			t.Fatalf("CNAME = %v", got.Answers[2].Target)
		}
		if got.Answers[3].Preference != 10 || got.Answers[3].Target != "mail.example.com." {
			t.Fatalf("MX = %+v", got.Answers[3])
		}
		if len(got.Answers[4].Text) != 3 { // 300-char string split into 255+45
			t.Fatalf("TXT chunks = %d (%q)", len(got.Answers[4].Text), got.Answers[4].Text)
		}
		if got.Answers[4].Text[1]+got.Answers[4].Text[2] != strings.Repeat("x", 300) {
			t.Fatalf("TXT reassembly mismatch")
		}
		srv := got.Answers[5]
		if srv.Priority != 10 || srv.Weight != 5 || srv.Port != 5060 || srv.Target != "sip.example.com." {
			t.Fatalf("SRV = %+v", srv)
		}
		soa := got.Authorities[0]
		if soa.Target != "ns1.example.com." || soa.Rname != "hostmaster.example.com." ||
			soa.Serial != 2024010101 || soa.Refresh != 7200 || soa.Retry != 3600 ||
			soa.Expire != 1209600 || soa.Minimum != 300 {
			t.Fatalf("SOA = %+v", soa)
		}
	}
}

func TestCompressionActuallyUsed(t *testing.T) {
	m := sampleMessage()
	withC, err := m.Pack(true)
	if err != nil {
		t.Fatal(err)
	}
	withoutC, err := m.Pack(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(withC) >= len(withoutC) {
		t.Fatalf("compression did not shrink message: %d vs %d", len(withC), len(withoutC))
	}
	if !bytes.Contains(withC, []byte{0xC0}) {
		t.Fatalf("no compression pointer found")
	}
}

func TestUnpackRejectsBadMessages(t *testing.T) {
	if _, err := Unpack([]byte{1, 2, 3}); err == nil {
		t.Fatal("short message accepted")
	}
	// Header claiming 1 question but no body.
	hdr := make([]byte, 12)
	hdr[5] = 1
	if _, err := Unpack(hdr); err == nil {
		t.Fatal("truncated question accepted")
	}
	// Forward compression pointer (loop).
	msg := make([]byte, 12)
	msg[5] = 1 // qdcount=1
	msg = append(msg, 0xC0, 12) // pointer to itself
	if _, err := Unpack(msg); err == nil {
		t.Fatal("compression loop accepted")
	}
	// Pointer beyond message.
	msg2 := make([]byte, 12)
	msg2[5] = 1
	msg2 = append(msg2, 0xC0, 0x7F)
	if _, err := Unpack(msg2); err == nil {
		t.Fatal("out-of-bounds pointer accepted")
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("WWW.Example.COM"); got != "www.example.com." {
		t.Fatalf("got %q", got)
	}
	if got := Normalize("."); got != "." {
		t.Fatalf("root: got %q", got)
	}
}
