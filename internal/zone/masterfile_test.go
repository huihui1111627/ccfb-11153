package zone

import (
	"strings"
	"testing"
)

const sampleZone = `$ORIGIN gym.example.
$TTL 300
@   IN  SOA ns1.gym.example. hostmaster.gym.example. (
        2024010101 ; serial
        7200       ; refresh
        3600       ; retry
        1209600    ; expire
        60         ; minimum
        )
@       IN  NS   ns1.gym.example.
@       IN  MX   10 mail.gym.example.
@       IN  TXT  "v=spf1 -all"
@       IN  ALIAS cdn-target.example.net.
www     IN  A    10.1.0.1 ; dc=sh weight=5
www     IN  A    10.2.0.1 ; dc=bj weight=3
www     60 A    10.3.0.1
api     IN  AAAA 2001:db8::10
mail    IN  A    10.1.0.25
_sip._tcp IN SRV 10 5 5060 sip.gym.example.
app     IN  CNAME www.gym.example.
`

func TestParseMaster(t *testing.T) {
	z, err := ParseMaster(sampleZone)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if z.Name != "gym.example" {
		t.Fatalf("zone name %q", z.Name)
	}
	if z.SOA.Serial != 2024010101 || z.SOA.Minimum != 60 || z.SOA.MName != "ns1.gym.example." {
		t.Fatalf("soa %+v", z.SOA)
	}
	www := z.RecordsAt("www.gym.example")
	if len(www) != 3 {
		t.Fatalf("www records = %d", len(www))
	}
	if www[0].DC != "sh" || www[0].Weight != 5 {
		t.Fatalf("metadata lost: %+v", www[0])
	}
	if www[2].TTL != 60 {
		t.Fatalf("inline ttl = %d", www[2].TTL)
	}
	apex := z.RecordsAt("gym.example")
	var sawNS, sawMX, sawTXT, sawALIAS bool
	for _, r := range apex {
		switch r.Type {
		case "NS":
			sawNS = true
		case "MX":
			sawMX = true
			if r.Value != "10 mail.gym.example." {
				t.Fatalf("mx value %q", r.Value)
			}
		case "TXT":
			sawTXT = true
		case TypeALIAS:
			sawALIAS = true
			if r.Value != "cdn-target.example.net." {
				t.Fatalf("alias target %q", r.Value)
			}
		}
	}
	if !sawNS || !sawMX || !sawTXT || !sawALIAS {
		t.Fatalf("apex records missing: %v", apex)
	}
	srv := z.RecordsAt("_sip._tcp.gym.example")
	if len(srv) != 1 || srv[0].Value != "10 5 5060 sip.gym.example." {
		t.Fatalf("srv %+v", srv)
	}
}

func TestExportRoundTrip(t *testing.T) {
	z, err := ParseMaster(sampleZone)
	if err != nil {
		t.Fatal(err)
	}
	out := ExportMaster(z)
	z2, err := ParseMaster(out)
	if err != nil {
		t.Fatalf("re-parse exported zone: %v\n%s", err, out)
	}
	if len(z2.RecordsAt("www.gym.example")) != 3 {
		t.Fatalf("round-trip lost records")
	}
	if z2.RecordsAt("www.gym.example")[0].DC != "sh" {
		t.Fatalf("round-trip lost dc metadata")
	}
	if !strings.Contains(out, "$ORIGIN gym.example.") {
		t.Fatalf("export missing origin:\n%s", out)
	}
}

func TestStoreOps(t *testing.T) {
	s := NewStore(nil)
	z := NewZone("gym.example")
	s.AddZone(z)
	if s.FindZone("www.gym.example") != z {
		t.Fatal("FindZone failed")
	}
	if s.FindZone("deep.sub.gym.example") != z {
		t.Fatal("FindZone suffix failed")
	}
	if s.FindZone("other.example") != nil {
		t.Fatal("FindZone false positive")
	}
	if err := s.AddRecord("gym.example", Record{Name: "www", Type: "A", Value: "1.2.3.4"}); err == nil {
		t.Fatal("relative name accepted") // names must be fqdn or @
	}
	if err := s.AddRecord("gym.example", Record{Name: "www.gym.example", Type: "A", Value: "1.2.3.4"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRecord("gym.example", Record{Name: "@", Type: "ALIAS", Value: "x.example.net."}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRecord("gym.example", Record{Name: "www.gym.example", Type: "ALIAS", Value: "x."}); err == nil {
		t.Fatal("ALIAS below apex accepted")
	}
	if err := s.DeleteRecord("gym.example", "www.gym.example", "A", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRecord("gym.example", "www.gym.example", "A", 0); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
