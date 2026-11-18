package server

import (
	"net"
	"strings"
	"testing"
	"time"

	"dnsd/internal/alias"
	"dnsd/internal/dnsmsg"
	"dnsd/internal/geo"
	"dnsd/internal/health"
	"dnsd/internal/ratelimit"
	"dnsd/internal/stats"
	"dnsd/internal/zone"
)

func newTestServer(t *testing.T) (*Server, *zone.Store, *health.Checker) {
	t.Helper()
	store := zone.NewStore(nil)
	z := zone.NewZone("gym.example")
	z.Fallback4 = "192.0.2.100"
	store.AddZone(z)
	recs := []zone.Record{
		{Name: "www.gym.example", Type: "A", Value: "10.1.0.1", DC: "sh", Weight: 5},
		{Name: "www.gym.example", Type: "A", Value: "10.2.0.1", DC: "bj", Weight: 3},
		{Name: "www.gym.example", Type: "A", Value: "10.3.0.1", DC: "gz", Weight: 2},
		{Name: "api.gym.example", Type: "AAAA", Value: "2001:db8::10"},
		{Name: "app.gym.example", Type: "CNAME", Value: "www.gym.example."},
		{Name: "gym.example", Type: "MX", Value: "10 mail.gym.example."},
	}
	for _, r := range recs {
		if err := store.AddRecord("gym.example", r); err != nil {
			t.Fatal(err)
		}
	}

	matcher := geo.NewMatcher([]geo.SubnetRule{
		{CIDR: "10.10.1.0/24", DC: "sh", Region: "华东"},
		{CIDR: "10.10.2.0/24", DC: "bj", Region: "华北"},
		{CIDR: "10.10.3.0/24", DC: "gz", Region: "华南"},
	})
	hc := health.New(time.Hour, time.Second, nil) // long interval: manual control
	rl := ratelimit.New(ratelimit.Policy{Enabled: false})
	st := stats.New(100)
	srv := New(store, matcher, hc, nil, rl, st)
	srv.DCWeights = map[string]int{"sh": 5, "bj": 3, "gz": 2}
	return srv, store, hc
}

func query(t *testing.T, srv *Server, name, typ string, clientIP string) *dnsmsg.Message {
	t.Helper()
	req := &dnsmsg.Message{
		Header:    dnsmsg.Header{ID: 1, RD: true},
		Questions: []dnsmsg.Question{{Name: dnsmsg.Normalize(name), Qtype: dnsmsg.NameToType[typ], Qclass: dnsmsg.ClassIN}},
	}
	wire, err := req.Pack(true)
	if err != nil {
		t.Fatal(err)
	}
	out := srv.ServeDNS(wire, net.ParseIP(clientIP), "udp")
	if out == nil {
		t.Fatal("response dropped")
	}
	resp, err := dnsmsg.Unpack(out)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestGeoRouting(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cases := map[string]string{
		"10.10.1.5": "10.1.0.1", // 华东 -> sh
		"10.10.2.5": "10.2.0.1", // 华北 -> bj
		"10.10.3.5": "10.3.0.1", // 华南 -> gz
	}
	for ip, want := range cases {
		resp := query(t, srv, "www.gym.example", "A", ip)
		if len(resp.Answers) != 1 || resp.Answers[0].IP.String() != want {
			t.Fatalf("client %s: got %+v, want %s", ip, resp.Answers, want)
		}
	}
}

func TestWeightedSplit(t *testing.T) {
	srv, _, _ := newTestServer(t)
	counts := map[string]int{}
	n := 3000
	for i := 0; i < n; i++ {
		resp := query(t, srv, "www.gym.example", "A", "8.8.8.8") // no geo match
		counts[resp.Answers[0].IP.String()]++
	}
	// Expect roughly 5:3:2.
	if counts["10.1.0.1"] < 1200 || counts["10.2.0.1"] < 600 || counts["10.3.0.1"] < 300 {
		t.Fatalf("skewed distribution: %v", counts)
	}
	if counts["10.1.0.1"] <= counts["10.3.0.1"] {
		t.Fatalf("weights not respected: %v", counts)
	}
}

func TestHealthFailoverAndFallback(t *testing.T) {
	srv, store, hc := newTestServer(t)
	// A record pointing at a guaranteed-closed local port.
	if err := store.AddRecord("gym.example", zone.Record{Name: "lb.gym.example", Type: "A", Value: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	hc.SetTargets([]health.Target{{IP: "127.0.0.1", Type: "tcp", Port: 1}})
	// Port 1 is closed: the target goes down after a probe.
	hc.ProbeNow()
	if hc.Healthy("127.0.0.1") {
		t.Fatal("probe should have marked 127.0.0.1:1 down")
	}
	resp := query(t, srv, "lb.gym.example", "A", "10.10.1.5")
	if len(resp.Answers) != 1 || resp.Answers[0].IP.String() != "192.0.2.100" {
		t.Fatalf("expected fallback 192.0.2.100, got %+v", resp.Answers)
	}

	// Remove fallback: all-down should yield SERVFAIL.
	z := store.Get("gym.example")
	z.Fallback4 = ""
	resp = query(t, srv, "lb.gym.example", "A", "10.10.1.5")
	if resp.Rcode != dnsmsg.RcodeServFail {
		t.Fatalf("expected SERVFAIL, got rcode %d", resp.Rcode)
	}
}

func TestNegativeResponses(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := query(t, srv, "nope.gym.example", "A", "10.10.1.5")
	if resp.Rcode != dnsmsg.RcodeNXDomain {
		t.Fatalf("expected NXDOMAIN, got %d", resp.Rcode)
	}
	if len(resp.Authorities) != 1 || resp.Authorities[0].Type != dnsmsg.TypeSOA {
		t.Fatalf("NXDOMAIN without SOA authority: %+v", resp.Authorities)
	}
	// NODATA: www exists but has no AAAA.
	resp = query(t, srv, "www.gym.example", "AAAA", "10.10.1.5")
	if resp.Rcode != dnsmsg.RcodeNoError || len(resp.Answers) != 0 {
		t.Fatalf("expected NODATA, got rcode=%d answers=%d", resp.Rcode, len(resp.Answers))
	}
	if len(resp.Authorities) != 1 || resp.Authorities[0].Type != dnsmsg.TypeSOA {
		t.Fatalf("NODATA without SOA authority")
	}
	// Out of zone: REFUSED.
	resp = query(t, srv, "other.example", "A", "10.10.1.5")
	if resp.Rcode != dnsmsg.RcodeRefused {
		t.Fatalf("expected REFUSED, got %d", resp.Rcode)
	}
}

func TestTruncation(t *testing.T) {
	srv, store, _ := newTestServer(t)
	for i := 0; i < 30; i++ {
		err := store.AddRecord("gym.example", zone.Record{
			Name: "big.gym.example", Type: "TXT",
			Value: strings.Repeat("x", 40) + "-" + strings.Repeat("y", 2) + string(rune('a'+i)),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// No EDNS -> 512 limit -> TC.
	req := &dnsmsg.Message{
		Header:    dnsmsg.Header{ID: 7, RD: true},
		Questions: []dnsmsg.Question{{Name: "big.gym.example.", Qtype: dnsmsg.TypeTXT, Qclass: dnsmsg.ClassIN}},
	}
	wire, _ := req.Pack(true)
	out := srv.ServeDNS(wire, net.ParseIP("10.10.1.5"), "udp")
	resp, _ := dnsmsg.Unpack(out)
	if !resp.TC {
		t.Fatal("expected TC flag on oversized UDP response")
	}
	if len(resp.Answers) != 0 {
		t.Fatal("truncated response should carry no answers")
	}
	// TCP -> full response, no TC.
	out = srv.ServeDNS(wire, net.ParseIP("10.10.1.5"), "tcp")
	resp, _ = dnsmsg.Unpack(out)
	if resp.TC || len(resp.Answers) != 30 {
		t.Fatalf("tcp: TC=%v answers=%d", resp.TC, len(resp.Answers))
	}
}

func TestRateLimitSlipAndDrop(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.RL.SetPolicy(ratelimit.Policy{
		Enabled: true, PrefixV4: 24, QPS: 1000, Burst: 1000,
		IdenticalMax: 3, IdenticalWindow: 60, Action: "slip",
	})
	req := &dnsmsg.Message{
		Header:    dnsmsg.Header{ID: 9, RD: true},
		Questions: []dnsmsg.Question{{Name: "www.gym.example.", Qtype: dnsmsg.TypeA, Qclass: dnsmsg.ClassIN}},
	}
	wire, _ := req.Pack(true)
	ip := net.ParseIP("10.10.1.5")
	sawSlip := false
	for i := 0; i < 10; i++ {
		out := srv.ServeDNS(wire, ip, "udp")
		if out == nil {
			t.Fatal("slip policy should not drop")
		}
		resp, _ := dnsmsg.Unpack(out)
		if resp.TC && len(resp.Answers) == 0 {
			sawSlip = true
		}
	}
	if !sawSlip {
		t.Fatal("identical flood was not slipped")
	}

	srv.RL.SetPolicy(ratelimit.Policy{
		Enabled: true, PrefixV4: 24, QPS: 1000, Burst: 1000,
		IdenticalMax: 3, IdenticalWindow: 60, Action: "drop",
	})
	sawDrop := false
	for i := 0; i < 10; i++ {
		if srv.ServeDNS(wire, net.ParseIP("10.10.2.5"), "udp") == nil {
			sawDrop = true
		}
	}
	if !sawDrop {
		t.Fatal("identical flood was not dropped")
	}

	// Subnet token bucket: tiny burst should exhaust quickly.
	srv.RL.SetPolicy(ratelimit.Policy{
		Enabled: true, PrefixV4: 24, QPS: 1, Burst: 2,
		IdenticalMax: 0, Action: "drop",
	})
	allowed := 0
	for i := 0; i < 10; i++ {
		if srv.ServeDNS(wire, net.ParseIP("10.10.7.7"), "udp") != nil {
			allowed++
		}
	}
	if allowed > 4 {
		t.Fatalf("token bucket not limiting: %d/10 allowed", allowed)
	}
}

func TestAliasResolution(t *testing.T) {
	// Stand up a second server as the "upstream recursor" for the target.
	upStore := zone.NewStore(nil)
	up := zone.NewZone("cdn.example")
	upStore.AddZone(up)
	if err := upStore.AddRecord("cdn.example", zone.Record{Name: "cdn.example", Type: "A", Value: "203.0.113.7", TTL: 30}); err != nil {
		t.Fatal(err)
	}
	upSrv := New(upStore, geo.NewMatcher(nil), nil, nil, ratelimit.New(ratelimit.Policy{Enabled: false}), stats.New(10))
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := ln.ReadFromUDP(buf)
			if err != nil {
				return
			}
			wire := append([]byte(nil), buf[:n]...)
			go func() {
				if out := upSrv.ServeDNS(wire, addr.IP, "udp"); out != nil {
					ln.WriteToUDP(out, addr)
				}
			}()
		}
	}()

	srv, store, _ := newTestServer(t)
	srv.Alias = alias.New([]string{ln.LocalAddr().String()}, time.Second)
	if err := store.AddRecord("gym.example", zone.Record{Name: "@", Type: zone.TypeALIAS, Value: "cdn.example."}); err != nil {
		t.Fatal(err)
	}
	resp := query(t, srv, "gym.example", "A", "10.10.1.5")
	if len(resp.Answers) != 1 || resp.Answers[0].IP.String() != "203.0.113.7" {
		t.Fatalf("ALIAS did not inline target: %+v", resp.Answers)
	}
	if resp.Answers[0].TTL > 30 {
		t.Fatalf("ALIAS TTL should track target TTL, got %d", resp.Answers[0].TTL)
	}
}

func TestCNAMEChase(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := query(t, srv, "app.gym.example", "A", "10.10.2.5")
	if len(resp.Answers) != 2 {
		t.Fatalf("expected CNAME + A, got %d answers", len(resp.Answers))
	}
	if resp.Answers[0].Type != dnsmsg.TypeCNAME || resp.Answers[1].Type != dnsmsg.TypeA {
		t.Fatalf("bad chase: %+v", resp.Answers)
	}
	if resp.Answers[1].IP.String() != "10.2.0.1" { // bj client
		t.Fatalf("chased A not geo-routed: %v", resp.Answers[1].IP)
	}
}
