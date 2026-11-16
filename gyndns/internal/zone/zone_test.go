package zone

import (
	"net"
	"strings"
	"testing"

	"gyndns/internal/dnsmsg"
)

const testZoneFile = `
$ORIGIN example.com.
$TTL 300
@	IN SOA ns1.example.com. hostmaster.example.com. (
		1 300 60 1209600 60 )
	IN NS ns1.example.com.
ns1	IN A 192.0.2.53
www	IN A 192.0.2.1
www	IN A 192.0.2.2
api	IN A 192.0.2.10
web	IN AAAA 2001:db8::1
mail	IN MX 10 mail1.example.com.
mail1	IN A 192.0.2.25
@	IN TXT "v=spf1 -all"
_sip._tcp IN SRV 10 60 5060 sip.example.com.
sip	IN A 192.0.2.30
alias	IN CNAME www.example.com.
loop1	IN CNAME loop2.example.com.
loop2	IN CNAME loop1.example.com.
chain1	IN CNAME chain2.example.com.
chain2	IN CNAME chain3.example.com.
chain3	IN CNAME chain4.example.com.
chain4	IN CNAME chain5.example.com.
chain5	IN CNAME chain6.example.com.
chain6	IN CNAME chain7.example.com.
chain7	IN CNAME chain8.example.com.
chain8	IN CNAME chain9.example.com.
chain9	IN A 192.0.2.99
ext	IN CNAME other.zone.
*.wild	IN A 192.0.2.100
`

func testStore(t *testing.T) *Store {
	t.Helper()
	z, err := ParseZoneFile(strings.NewReader(testZoneFile))
	if err != nil {
		t.Fatalf("parse zone: %v", err)
	}
	if z.Name != "example.com." {
		t.Fatalf("zone name %q", z.Name)
	}
	s := NewStore()
	s.ReplaceZone(z, false)
	return s
}

var clientIP = net.ParseIP("203.0.113.7")

func TestZoneFileParse(t *testing.T) {
	s := testStore(t)
	z := s.GetZone("example.com")
	if z == nil {
		t.Fatal("zone not found")
	}
	if z.Serial() != 1 {
		t.Fatalf("serial %d", z.Serial())
	}
	// every record must convert to wire format
	for _, r := range z.Records {
		if _, err := r.RR(""); err != nil {
			t.Fatalf("record %s: %v", r.Key(), err)
		}
	}
}

func TestZoneFileExportRoundTrip(t *testing.T) {
	s := testStore(t)
	z := s.GetZone("example.com.")
	out := ExportZoneFile(z)
	z2, err := ParseZoneFile(strings.NewReader(out))
	if err != nil {
		t.Fatalf("re-parse exported zone: %v\n%s", err, out)
	}
	if len(z2.Records) != len(z.Records) {
		t.Fatalf("record count %d != %d\n%s", len(z2.Records), len(z.Records), out)
	}
	if z2.Name != z.Name || z2.Serial() != z.Serial() {
		t.Fatal("zone metadata lost in round trip")
	}
}

func TestResolveA(t *testing.T) {
	s := testStore(t)
	res := s.Resolve("www.example.com", dnsmsg.TypeA, clientIP)
	if res.RCode != dnsmsg.RcodeSuccess || len(res.Answers) != 2 {
		t.Fatalf("rcode=%d answers=%d", res.RCode, len(res.Answers))
	}
	if res.Answers[0].Name != "www.example.com." {
		t.Fatalf("owner %q", res.Answers[0].Name)
	}
}

func TestResolveAllTypes(t *testing.T) {
	s := testStore(t)
	cases := []struct {
		name  string
		qtype uint16
		want  int
	}{
		{"www.example.com.", dnsmsg.TypeA, 2},
		{"web.example.com.", dnsmsg.TypeAAAA, 1},
		{"mail.example.com.", dnsmsg.TypeMX, 1},
		{"example.com.", dnsmsg.TypeTXT, 1},
		{"_sip._tcp.example.com.", dnsmsg.TypeSRV, 1},
		{"example.com.", dnsmsg.TypeNS, 1},
		{"example.com.", dnsmsg.TypeSOA, 1},
	}
	for _, c := range cases {
		res := s.Resolve(c.name, c.qtype, clientIP)
		if res.RCode != dnsmsg.RcodeSuccess || len(res.Answers) != c.want {
			t.Errorf("%s %s: rcode=%s answers=%d want %d",
				c.name, dnsmsg.TypeString(c.qtype), dnsmsg.RcodeString(res.RCode), len(res.Answers), c.want)
		}
	}
}

func TestResolveNXDOMAIN(t *testing.T) {
	s := testStore(t)
	res := s.Resolve("nosuch.example.com.", dnsmsg.TypeA, clientIP)
	if res.RCode != dnsmsg.RcodeNameError {
		t.Fatalf("rcode=%s", dnsmsg.RcodeString(res.RCode))
	}
	if len(res.Authorities) != 1 || res.Authorities[0].Type != dnsmsg.TypeSOA {
		t.Fatal("NXDOMAIN must carry SOA in authority section")
	}
	soa := res.Authorities[0].Data.(*dnsmsg.SOA)
	if res.Authorities[0].TTL != uint32(60) || soa.MinTTL != 60 {
		t.Fatalf("negative TTL should be SOA minimum: %+v", res.Authorities[0])
	}
}

func TestResolveNODATA(t *testing.T) {
	s := testStore(t)
	res := s.Resolve("www.example.com.", dnsmsg.TypeMX, clientIP)
	if res.RCode != dnsmsg.RcodeSuccess || len(res.Answers) != 0 {
		t.Fatalf("NODATA: rcode=%s answers=%d", dnsmsg.RcodeString(res.RCode), len(res.Answers))
	}
	if len(res.Authorities) != 1 || res.Authorities[0].Type != dnsmsg.TypeSOA {
		t.Fatal("NODATA must carry SOA in authority section")
	}
}

func TestResolveWildcard(t *testing.T) {
	s := testStore(t)
	res := s.Resolve("anything.wild.example.com.", dnsmsg.TypeA, clientIP)
	if res.RCode != dnsmsg.RcodeSuccess || len(res.Answers) != 1 {
		t.Fatalf("wildcard: rcode=%s answers=%d", dnsmsg.RcodeString(res.RCode), len(res.Answers))
	}
	// owner name must be the query name, not *.wild
	if res.Answers[0].Name != "anything.wild.example.com." {
		t.Fatalf("wildcard owner %q", res.Answers[0].Name)
	}
	if res.Answers[0].Data.String() != "192.0.2.100" {
		t.Fatalf("wildcard rdata %q", res.Answers[0].Data.String())
	}
}

func TestResolveCNAME(t *testing.T) {
	s := testStore(t)
	res := s.Resolve("alias.example.com.", dnsmsg.TypeA, clientIP)
	if res.RCode != dnsmsg.RcodeSuccess {
		t.Fatalf("rcode=%s", dnsmsg.RcodeString(res.RCode))
	}
	if len(res.Answers) != 3 { // 1 CNAME + 2 A
		t.Fatalf("answers=%d", len(res.Answers))
	}
	if res.Answers[0].Type != dnsmsg.TypeCNAME {
		t.Fatalf("first answer should be CNAME, got %s", dnsmsg.TypeString(res.Answers[0].Type))
	}
}

func TestResolveCNAMEOutOfZone(t *testing.T) {
	s := testStore(t)
	res := s.Resolve("ext.example.com.", dnsmsg.TypeA, clientIP)
	if res.RCode != dnsmsg.RcodeSuccess || len(res.Answers) != 1 {
		t.Fatalf("rcode=%s answers=%d", dnsmsg.RcodeString(res.RCode), len(res.Answers))
	}
	if res.Answers[0].Type != dnsmsg.TypeCNAME {
		t.Fatal("out-of-zone CNAME target must not be chased")
	}
}

func TestResolveCNAMELoop(t *testing.T) {
	s := testStore(t)
	res := s.Resolve("loop1.example.com.", dnsmsg.TypeA, clientIP)
	if res.RCode != dnsmsg.RcodeServerFailure {
		t.Fatalf("CNAME loop should SERVFAIL, got %s", dnsmsg.RcodeString(res.RCode))
	}
	found := false
	for _, tr := range res.Trace {
		if strings.Contains(tr, "成环") {
			found = true
		}
	}
	if !found {
		t.Fatalf("trace should mention the loop: %v", res.Trace)
	}
}

func TestResolveCNAMEChainTooLong(t *testing.T) {
	s := testStore(t)
	res := s.Resolve("chain1.example.com.", dnsmsg.TypeA, clientIP)
	if res.RCode != dnsmsg.RcodeServerFailure {
		t.Fatalf("long chain should SERVFAIL, got %s", dnsmsg.RcodeString(res.RCode))
	}
}

func TestResolveRefusedOutOfZone(t *testing.T) {
	s := testStore(t)
	res := s.Resolve("www.other.com.", dnsmsg.TypeA, clientIP)
	if res.RCode != dnsmsg.RcodeRefused {
		t.Fatalf("rcode=%s", dnsmsg.RcodeString(res.RCode))
	}
}

func TestMutationsBumpSerialAndJournal(t *testing.T) {
	s := testStore(t)
	if err := s.AddRecord("example.com.", Record{
		Name: "new.example.com.", Type: dnsmsg.TypeA, TTL: 60, RData: "192.0.2.77",
	}); err != nil {
		t.Fatal(err)
	}
	z := s.GetZone("example.com.")
	if z.Serial() != 2 {
		t.Fatalf("serial %d after add", z.Serial())
	}
	j := s.Journal("example.com.")
	if len(j) != 1 || j[0].Serial != 2 || len(j[0].Add) != 1 {
		t.Fatalf("journal %+v", j)
	}
	if err := s.DeleteRecord("example.com.", "new.example.com.", dnsmsg.TypeA, ""); err != nil {
		t.Fatal(err)
	}
	if z.Serial() != 3 {
		t.Fatalf("serial %d after delete", z.Serial())
	}
	res := s.Resolve("new.example.com.", dnsmsg.TypeA, clientIP)
	if res.RCode != dnsmsg.RcodeNameError {
		t.Fatal("deleted record still resolves")
	}
}

func TestApplyIXFR(t *testing.T) {
	master := testStore(t)
	slave := testStore(t)
	if err := master.AddRecord("example.com.", Record{
		Name: "dyn.example.com.", Type: dnsmsg.TypeA, TTL: 60, RData: "192.0.2.88",
	}); err != nil {
		t.Fatal(err)
	}
	j := master.Journal("example.com.")
	if err := slave.ApplyIXFR("example.com.", j); err != nil {
		t.Fatal(err)
	}
	res := slave.Resolve("dyn.example.com.", dnsmsg.TypeA, clientIP)
	if res.RCode != dnsmsg.RcodeSuccess || len(res.Answers) != 1 {
		t.Fatal("IXFR change not applied on slave")
	}
	if slave.GetZone("example.com.").Serial() != 2 {
		t.Fatalf("slave serial %d", slave.GetZone("example.com.").Serial())
	}
}
