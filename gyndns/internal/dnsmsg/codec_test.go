package dnsmsg

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

func mkQuery(name string, qtype uint16) *Message {
	return &Message{
		Header:    Header{ID: 0x1234, RD: true},
		Questions: []Question{{Name: name, Type: qtype, Class: ClassIN}},
	}
}

func roundTrip(t *testing.T, m *Message) *Message {
	t.Helper()
	buf, err := m.Pack(0)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	got, err := Unpack(buf)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	return got
}

func TestHeaderRoundTrip(t *testing.T) {
	q := mkQuery("www.example.com.", TypeA)
	r := NewReply(q)
	r.RCode = RcodeNameError
	got := roundTrip(t, r)
	if got.ID != 0x1234 || !got.QR || !got.RD || got.RCode != RcodeNameError {
		t.Fatalf("header mismatch: %+v", got.Header)
	}
	if len(got.Questions) != 1 || got.Questions[0].Name != "www.example.com." {
		t.Fatalf("question mismatch: %+v", got.Questions)
	}
}

func TestAllRDataTypesRoundTrip(t *testing.T) {
	m := mkQuery("example.com.", TypeANY)
	m.Answers = []RR{
		{Name: "example.com.", Type: TypeA, Class: ClassIN, TTL: 300, Data: &A{IP: net.ParseIP("192.0.2.1")}},
		{Name: "example.com.", Type: TypeAAAA, Class: ClassIN, TTL: 300, Data: &AAAA{IP: net.ParseIP("2001:db8::1")}},
		{Name: "example.com.", Type: TypeNS, Class: ClassIN, TTL: 300, Data: &NS{Host: "ns1.example.com."}},
		{Name: "www.example.com.", Type: TypeCNAME, Class: ClassIN, TTL: 300, Data: &CNAME{Target: "web.example.com."}},
		{Name: "example.com.", Type: TypeMX, Class: ClassIN, TTL: 300, Data: &MX{Preference: 10, Host: "mail.example.com."}},
		{Name: "example.com.", Type: TypeTXT, Class: ClassIN, TTL: 300, Data: &TXT{Texts: []string{"v=spf1 -all", "second"}}},
		{Name: "_sip._tcp.example.com.", Type: TypeSRV, Class: ClassIN, TTL: 300, Data: &SRV{Priority: 10, Weight: 60, Port: 5060, Target: "sip.example.com."}},
		{Name: "example.com.", Type: TypeSOA, Class: ClassIN, TTL: 300, Data: &SOA{NS: "ns1.example.com.", Mbox: "hostmaster.example.com.", Serial: 2024010101, Refresh: 7200, Retry: 3600, Expire: 1209600, MinTTL: 300}},
	}
	got := roundTrip(t, m)
	if len(got.Answers) != len(m.Answers) {
		t.Fatalf("answer count %d != %d", len(got.Answers), len(m.Answers))
	}
	for i, want := range m.Answers {
		g := got.Answers[i]
		if g.Name != want.Name || g.Type != want.Type || g.TTL != want.TTL {
			t.Fatalf("rr %d header mismatch: %+v vs %+v", i, g, want)
		}
		if g.Data.String() != want.Data.String() {
			t.Fatalf("rr %d rdata mismatch: %q vs %q", i, g.Data.String(), want.Data.String())
		}
	}
}

func TestCompressionActuallyUsed(t *testing.T) {
	m := mkQuery("example.com.", TypeA)
	for i := 0; i < 5; i++ {
		m.Answers = append(m.Answers, RR{
			Name: "example.com.", Type: TypeA, Class: ClassIN, TTL: 60,
			Data: &A{IP: net.ParseIP("192.0.2.1").To4()},
		})
	}
	buf, err := m.Pack(0)
	if err != nil {
		t.Fatal(err)
	}
	// 5 uncompressed owner names would cost 13 bytes each; with compression
	// all but the first cost 2 bytes. Uncompressed total would be ~164 bytes.
	if len(buf) > 140 {
		t.Fatalf("message too large, compression not applied: %d bytes", len(buf))
	}
	if !bytes.Contains(buf, []byte{0xC0, 0x0C}) {
		t.Fatal("no compression pointer to offset 12 found")
	}
	if _, err := Unpack(buf); err != nil {
		t.Fatalf("re-unpack: %v", err)
	}
}

func TestTruncationSetsTC(t *testing.T) {
	m := mkQuery("example.com.", TypeTXT)
	for i := 0; i < 20; i++ {
		m.Answers = append(m.Answers, RR{
			Name: "example.com.", Type: TypeTXT, Class: ClassIN, TTL: 60,
			Data: &TXT{Texts: []string{strings.Repeat("x", 100)}},
		})
	}
	full, err := m.Pack(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) < 2000 {
		t.Fatalf("expected large message, got %d", len(full))
	}
	buf, err := m.Pack(512)
	if err != nil {
		t.Fatal(err)
	}
	if len(buf) > 512 {
		t.Fatalf("truncated message exceeds limit: %d", len(buf))
	}
	got, err := Unpack(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TC {
		t.Fatal("TC bit not set on truncated message")
	}
	if len(got.Answers) >= 20 {
		t.Fatalf("expected fewer answers after truncation, got %d", len(got.Answers))
	}
	// TCP path: no limit, full data, no TC.
	if got2 := roundTrip(t, m); got2.TC || len(got2.Answers) != 20 {
		t.Fatal("full pack should carry all answers without TC")
	}
}

func TestUnpackRejectsPointerLoop(t *testing.T) {
	// header + question whose name is a pointer to itself (offset 12).
	buf := make([]byte, 12+2+4)
	binary.BigEndian.PutUint16(buf[4:], 1) // qdcount
	buf[12] = 0xC0
	buf[13] = 0x0C // pointer to offset 12 (itself)
	binary.BigEndian.PutUint16(buf[14:], TypeA)
	binary.BigEndian.PutUint16(buf[16:], ClassIN)
	if _, err := Unpack(buf); err == nil {
		t.Fatal("expected error for pointer loop")
	}
}

func TestUnpackRejectsForwardPointer(t *testing.T) {
	buf := make([]byte, 12+2+4+2)
	binary.BigEndian.PutUint16(buf[4:], 1)
	buf[12] = 0xC0
	buf[13] = 0x14 // pointer forward to offset 20
	binary.BigEndian.PutUint16(buf[14:], TypeA)
	binary.BigEndian.PutUint16(buf[16:], ClassIN)
	if _, err := Unpack(buf); err == nil {
		t.Fatal("expected error for forward pointer")
	}
}

func TestUnpackRejectsTruncated(t *testing.T) {
	m := mkQuery("www.example.com.", TypeA)
	m.Answers = []RR{{Name: "www.example.com.", Type: TypeA, Class: ClassIN, TTL: 1, Data: &A{IP: net.ParseIP("1.2.3.4")}}}
	buf, err := m.Pack(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 5, 11, 12, 20, len(buf) - 1} {
		if _, err := Unpack(buf[:n]); err == nil {
			t.Fatalf("expected error unpacking %d/%d bytes", n, len(buf))
		}
	}
}

func TestUnpackRejectsTrailingGarbage(t *testing.T) {
	m := mkQuery("example.com.", TypeA)
	buf, _ := m.Pack(0)
	buf = append(buf, 0xFF)
	if _, err := Unpack(buf); err == nil {
		t.Fatal("expected error for trailing bytes")
	}
}

func TestEDNSAndECS(t *testing.T) {
	m := mkQuery("example.com.", TypeA)
	m.SetEDNS(1232)
	opt := m.Additionals[0].Data.(*OPT)
	opt.Options = append(opt.Options, EDNSOption{
		Code: EDNSOptionECS,
		Data: []byte{0, 1, 24, 0, 203, 0, 113}, // family 1, /24, 203.0.113.0
	})
	got := roundTrip(t, m)
	if got.UDPSize() != 1232 {
		t.Fatalf("udp size %d", got.UDPSize())
	}
	cs := got.ClientSubnet()
	if cs == nil || cs.String() != "203.0.113.0/24" {
		t.Fatalf("ecs mismatch: %v", cs)
	}
	plain := roundTrip(t, mkQuery("example.com.", TypeA))
	if plain.UDPSize() != 512 {
		t.Fatalf("default udp size %d", plain.UDPSize())
	}
}

func TestParseRDataString(t *testing.T) {
	cases := []struct {
		typ  uint16
		in   string
		want string
	}{
		{TypeA, "192.0.2.1", "192.0.2.1"},
		{TypeAAAA, "2001:db8::1", "2001:db8::1"},
		{TypeMX, "10 mail.example.com.", "10 mail.example.com."},
		{TypeTXT, `"hello world"`, `"hello world"`},
		{TypeSRV, "10 60 5060 sip.example.com.", "10 60 5060 sip.example.com."},
		{TypeSOA, "ns1.example.com. hostmaster.example.com. 1 7200 3600 1209600 300", "ns1.example.com. hostmaster.example.com. 1 7200 3600 1209600 300"},
	}
	for _, c := range cases {
		d, err := ParseRDataString(c.typ, c.in)
		if err != nil {
			t.Fatalf("%s %q: %v", TypeString(c.typ), c.in, err)
		}
		if d.String() != c.want {
			t.Fatalf("%s %q: got %q", TypeString(c.typ), c.in, d.String())
		}
	}
	if _, err := ParseRDataString(TypeA, "not-an-ip"); err == nil {
		t.Fatal("expected error for bad A rdata")
	}
}

func TestRootAndLongNames(t *testing.T) {
	m := mkQuery(".", TypeNS)
	if got := roundTrip(t, m); got.Questions[0].Name != "." {
		t.Fatalf("root name: %q", got.Questions[0].Name)
	}
	// 253-char name: four 63-char labels + dots.
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." +
		strings.Repeat("c", 63) + "." + strings.Repeat("d", 61) + "."
	m2 := mkQuery(long, TypeA)
	if got := roundTrip(t, m2); got.Questions[0].Name != long {
		t.Fatalf("long name round trip failed")
	}
	// Label of 64 bytes must fail to pack.
	bad := mkQuery(strings.Repeat("x", 64)+".com.", TypeA)
	if _, err := bad.Pack(0); err == nil {
		t.Fatal("expected error for overlong label")
	}
}
