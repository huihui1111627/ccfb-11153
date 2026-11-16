package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"gyndns/internal/dnsmsg"
	"gyndns/internal/health"
	"gyndns/internal/qlog"
	"gyndns/internal/router"
	"gyndns/internal/xfr"
	"gyndns/internal/zone"
)

const e2eZone = `
$ORIGIN gym.example.
$TTL 300
@ IN SOA ns1.gym.example. hostmaster.gym.example. ( 1 300 60 1209600 60 )
@ IN NS ns1.gym.example.
app IN A 10.1.0.10
app IN A 10.2.0.10
big IN TXT "` + "%s" + `"
big IN TXT "` + "%s" + `"
big IN TXT "` + "%s" + `"
big IN TXT "` + "%s" + `"
big IN TXT "` + "%s" + `"
big IN TXT "` + "%s" + `"
`

func buildStore(t *testing.T) *zone.Store {
	t.Helper()
	txt := strings.Repeat("x", 100)
	zf := fmt.Sprintf(e2eZone, txt, txt, txt, txt, txt, txt)
	z, err := zone.ParseZoneFile(strings.NewReader(zf))
	if err != nil {
		t.Fatal(err)
	}
	s := zone.NewStore()
	s.ReplaceZone(z, false)
	return s
}

func startServer(t *testing.T, store *zone.Store, hc *health.Checker) (addr string, srv *Server, stop func()) {
	t.Helper()
	store.Selector = router.New(hc)
	ql := qlog.New(1000)
	srv = New(store, ql)
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()
	addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	errCh := make(chan error, 2)
	go func() { errCh <- srv.ServeUDP(ctx, addr) }()
	go func() { errCh <- srv.ServeTCP(ctx, addr) }()
	// wait for the sockets to come up
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("udp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return addr, srv, func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(time.Second):
		}
	}
}

func udpQuery(t *testing.T, addr string, m *dnsmsg.Message) *dnsmsg.Message {
	t.Helper()
	buf, err := m.Pack(0)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("udp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(buf); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 65535)
	n, err := conn.Read(resp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dnsmsg.Unpack(resp[:n])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func tcpQuery(t *testing.T, addr string, m *dnsmsg.Message) *dnsmsg.Message {
	t.Helper()
	buf, err := m.Pack(0)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(append([]byte{byte(len(buf) >> 8), byte(len(buf))}, buf...)); err != nil {
		t.Fatal(err)
	}
	var lbuf [2]byte
	if _, err := io.ReadFull(conn, lbuf[:]); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, binary.BigEndian.Uint16(lbuf[:]))
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatal(err)
	}
	got, err := dnsmsg.Unpack(resp)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func query(name string, qtype uint16) *dnsmsg.Message {
	return &dnsmsg.Message{
		Header:    dnsmsg.Header{ID: 42, RD: true},
		Questions: []dnsmsg.Question{{Name: name, Type: qtype, Class: dnsmsg.ClassIN}},
	}
}

func TestUDPBasicQuery(t *testing.T) {
	store := buildStore(t)
	addr, _, stop := startServer(t, store, nil)
	defer stop()
	got := udpQuery(t, addr, query("app.gym.example.", dnsmsg.TypeA))
	if got.RCode != dnsmsg.RcodeSuccess || len(got.Answers) != 1 {
		t.Fatalf("rcode=%d answers=%d", got.RCode, len(got.Answers))
	}
	if !got.AA {
		t.Fatal("AA bit should be set")
	}
}

func TestUDPTruncationAndTCPRetry(t *testing.T) {
	store := buildStore(t)
	addr, _, stop := startServer(t, store, nil)
	defer stop()
	// no EDNS: 512 limit, big TXT set must truncate
	got := udpQuery(t, addr, query("big.gym.example.", dnsmsg.TypeTXT))
	if !got.TC {
		t.Fatalf("expected TC on 512-byte-limited answer, got %d answers", len(got.Answers))
	}
	// over TCP the full set arrives without TC
	full := tcpQuery(t, addr, query("big.gym.example.", dnsmsg.TypeTXT))
	if full.TC {
		t.Fatal("TCP answer must not be truncated")
	}
	if len(full.Answers) != 6 {
		t.Fatalf("TCP answers=%d want 6", len(full.Answers))
	}
	if len(full.Answers) <= len(got.Answers) {
		t.Fatalf("TCP should carry more answers than truncated UDP (%d vs %d)",
			len(full.Answers), len(got.Answers))
	}
}

func TestEDNSRaisesLimit(t *testing.T) {
	store := buildStore(t)
	addr, _, stop := startServer(t, store, nil)
	defer stop()
	q := query("big.gym.example.", dnsmsg.TypeTXT)
	q.SetEDNS(1232)
	got := udpQuery(t, addr, q)
	if got.TC {
		t.Fatal("1232-byte EDNS answer should fit without truncation")
	}
	if len(got.Answers) != 6 {
		t.Fatalf("answers=%d", len(got.Answers))
	}
	if !got.HasEDNS() {
		t.Fatal("response should carry EDNS")
	}
}

func TestSubnetRoutingOverECS(t *testing.T) {
	store := buildStore(t)
	// add subnet-scoped records
	store.AddRecord("gym.example.", zone.Record{
		Name: "app.gym.example.", Type: dnsmsg.TypeA, TTL: 60,
		RData: "10.9.0.1", Subnets: []string{"10.9.0.0/16"},
	})
	addr, _, stop := startServer(t, store, nil)
	defer stop()
	q := query("app.gym.example.", dnsmsg.TypeA)
	q.SetEDNS(1232)
	opt := q.Additionals[0].Data.(*dnsmsg.OPT)
	opt.Options = append(opt.Options, dnsmsg.EDNSOption{
		Code: dnsmsg.EDNSOptionECS,
		Data: []byte{0, 1, 16, 0, 10, 9}, // 10.9.0.0/16
	})
	got := udpQuery(t, addr, q)
	if len(got.Answers) != 1 || got.Answers[0].Data.String() != "10.9.0.1" {
		t.Fatalf("ECS routing failed: %v", got.Answers)
	}
}

func TestHealthFailover(t *testing.T) {
	store := buildStore(t)
	hc := health.New(50 * time.Millisecond)
	// point both app records at checks, one forced down
	store.AddRecord("gym.example.", zone.Record{
		Name: "app.gym.example.", Type: dnsmsg.TypeA, TTL: 60,
		RData: "203.0.113.9", Backup: true,
	})
	z := store.GetZone("gym.example.")
	for _, r := range z.Records {
		if r.Name == "app.gym.example." && !r.Backup {
			down := false
			hc.SetOverride(r.Key(), &down)
		}
	}
	addr, _, stop := startServer(t, store, hc)
	defer stop()
	got := udpQuery(t, addr, query("app.gym.example.", dnsmsg.TypeA))
	if len(got.Answers) != 1 || got.Answers[0].Data.String() != "203.0.113.9" {
		t.Fatalf("expected backup address, got %v", got.Answers)
	}
}

func TestMasterSlaveSync(t *testing.T) {
	masterStore := buildStore(t)
	masterAddr, masterSrv, stopMaster := startServer(t, masterStore, nil)
	defer stopMaster()
	master := xfr.NewMaster(masterStore, nil)
	masterSrv.Master = master

	slaveStore := zone.NewStore()
	slaveAddr, slaveSrv, stopSlave := startServer(t, slaveStore, nil)
	defer stopSlave()
	slave := xfr.NewSlave(slaveStore, masterAddr)
	slaveSrv.Slave = slave

	// initial transfer = AXFR fallback (slave has no serial)
	if err := slave.Transfer("gym.example."); err != nil {
		t.Fatalf("initial transfer: %v", err)
	}
	if slaveStore.GetZone("gym.example.") == nil {
		t.Fatal("slave has no zone after transfer")
	}
	got := udpQuery(t, slaveAddr, query("app.gym.example.", dnsmsg.TypeA))
	if got.RCode != dnsmsg.RcodeSuccess || len(got.Answers) == 0 {
		t.Fatal("slave does not answer after transfer")
	}

	// change on master -> IXFR via journal
	if err := masterStore.AddRecord("gym.example.", zone.Record{
		Name: "new.gym.example.", Type: dnsmsg.TypeA, TTL: 60, RData: "10.7.7.7",
	}); err != nil {
		t.Fatal(err)
	}
	if err := slave.Transfer("gym.example."); err != nil {
		t.Fatalf("ixfr transfer: %v", err)
	}
	got = udpQuery(t, slaveAddr, query("new.gym.example.", dnsmsg.TypeA))
	if len(got.Answers) != 1 || got.Answers[0].Data.String() != "10.7.7.7" {
		t.Fatalf("IXFR change missing on slave: %v", got.Answers)
	}
	if slaveStore.GetZone("gym.example.").Serial() != masterStore.GetZone("gym.example.").Serial() {
		t.Fatalf("serials differ: slave %d master %d",
			slaveStore.GetZone("gym.example.").Serial(), masterStore.GetZone("gym.example.").Serial())
	}

	// NOTIFY: master notifies, slave transfers on its own
	master.Slaves = []string{slaveAddr}
	master.NotifySlaves("gym.example.", 0)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(slave.Status()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(slave.Status()) == 0 {
		t.Fatal("slave did not react to NOTIFY")
	}
}

func TestAXFRQueryOverTCP(t *testing.T) {
	store := buildStore(t)
	addr, srv, stop := startServer(t, store, nil)
	defer stop()
	srv.Master = xfr.NewMaster(store, nil)
	q := query("gym.example.", dnsmsg.TypeAXFR)
	got := tcpQuery(t, addr, q)
	if len(got.Answers) < 3 {
		t.Fatalf("AXFR answers=%d", len(got.Answers))
	}
	if got.Answers[0].Type != dnsmsg.TypeSOA || got.Answers[len(got.Answers)-1].Type != dnsmsg.TypeSOA {
		t.Fatal("AXFR must start and end with SOA")
	}
	// AXFR over UDP must be refused
	if got := udpQuery(t, addr, q); got.RCode != dnsmsg.RcodeRefused {
		t.Fatalf("AXFR over UDP: rcode=%d", got.RCode)
	}
}

func TestQueryLogRecorded(t *testing.T) {
	store := buildStore(t)
	addr, srv, stop := startServer(t, store, nil)
	defer stop()
	udpQuery(t, addr, query("app.gym.example.", dnsmsg.TypeA))
	entries := srv.Log.Query("app.gym.example", "", 10)
	if len(entries) != 1 {
		t.Fatalf("log entries=%d", len(entries))
	}
	e := entries[0]
	if e.RCode != "NOERROR" || len(e.Answers) == 0 || len(e.Trace) == 0 {
		t.Fatalf("bad log entry: %+v", e)
	}
}
