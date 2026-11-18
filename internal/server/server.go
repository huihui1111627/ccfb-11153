// Package server wires the DNS query pipeline: parse, rate-limit,
// resolve (geo/weight/health/alias), truncate, log.
package server

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"dnsd/internal/alias"
	"dnsd/internal/dnsmsg"
	"dnsd/internal/geo"
	"dnsd/internal/health"
	"dnsd/internal/ratelimit"
	"dnsd/internal/stats"
	"dnsd/internal/zone"
)

// Server is the authoritative DNS server.
type Server struct {
	Store     *zone.Store
	Geo       *geo.Matcher
	Health    *health.Checker
	Alias     *alias.Resolver
	RL        *ratelimit.Limiter
	Stats     *stats.Stats
	DCWeights map[string]int // datacenter -> weight for traffic split
	UDPSize   int            // max UDP response we send without truncation
}

// New builds a server with defaults.
func New(store *zone.Store, g *geo.Matcher, hc *health.Checker, al *alias.Resolver, rl *ratelimit.Limiter, st *stats.Stats) *Server {
	return &Server{
		Store: store, Geo: g, Health: hc, Alias: al, RL: rl, Stats: st,
		DCWeights: map[string]int{},
		UDPSize:   1232,
	}
}

// ServeDNS handles one wire-format query and returns the wire-format
// response, or nil to drop it (rate limiting).
func (s *Server) ServeDNS(wire []byte, clientIP net.IP, proto string) []byte {
	start := time.Now()
	pol := s.RL.Policy()
	subnet := geo.SubnetKey(clientIP, pol.PrefixV4, pol.PrefixV6)
	_, region, _ := s.Geo.Lookup(clientIP)

	req, err := dnsmsg.Unpack(wire)
	if err != nil {
		// FORMERR with whatever ID we could salvage.
		resp := &dnsmsg.Message{}
		if len(wire) >= 2 {
			resp.ID = binary.BigEndian.Uint16(wire)
		}
		resp.QR = true
		resp.Rcode = dnsmsg.RcodeFormErr
		out, _ := resp.Pack(false)
		return out
	}

	// Per-subnet token bucket.
	if !s.RL.CheckSubnet(subnet) {
		s.log(start, clientIP, subnet, region, req, nil, proto, "DROP", "", true, false, true)
		return nil
	}

	resp, dc := s.resolve(req, clientIP)

	// Identical-response limiting (anti reflection/amplification).
	var key uint64
	{
		h := fnv.New64a()
		for _, q := range req.Questions {
			fmt.Fprintf(h, "%s/%d/", q.Name, q.Qtype)
		}
		for _, rr := range resp.Answers {
			fmt.Fprintf(h, "%s;", rr.String())
		}
		key = h.Sum64()
	}
	slipped := false
	switch s.RL.CheckIdentical(subnet, key) {
	case ratelimit.Drop:
		s.log(start, clientIP, subnet, region, req, resp, proto, "DROP", dc, true, false, true)
		return nil
	case ratelimit.Slip:
		slipped = true
		out := truncatedResponse(req, resp)
		s.log(start, clientIP, subnet, region, req, resp, proto, "SLIP", dc, false, true, false)
		return out
	}

	out, truncated := s.packForTransport(req, resp, proto)
	rcode := rcodeName(resp.Rcode)
	s.log(start, clientIP, subnet, region, req, resp, proto, rcode, dc, false, slipped, truncated)
	return out
}

// packForTransport packs the response, applying UDP size limits.
func (s *Server) packForTransport(req, resp *dnsmsg.Message, proto string) ([]byte, bool) {
	resp.EDNSSize = req.EDNSSize // echo OPT
	out, err := resp.Pack(true)
	if err != nil {
		srvfail := &dnsmsg.Message{Questions: req.Questions}
		srvfail.ID, srvfail.QR, srvfail.RD = req.ID, true, req.RD
		srvfail.Rcode = dnsmsg.RcodeServFail
		out, _ = srvfail.Pack(false)
		return out, false
	}
	if proto == "tcp" {
		return out, false
	}
	limit := 512
	if req.EDNSSize > 0 {
		limit = req.EDNSSize
		if limit > 4096 {
			limit = 4096
		}
	}
	if s.UDPSize > 0 && s.UDPSize < limit {
		limit = s.UDPSize
	}
	if len(out) <= limit {
		return out, false
	}
	return truncatedResponse(req, resp), true
}

// truncatedResponse builds a TC=1 response carrying only the question.
func truncatedResponse(req, resp *dnsmsg.Message) []byte {
	t := &dnsmsg.Message{Questions: req.Questions}
	t.ID = req.ID
	t.QR = true
	t.AA = resp.AA
	t.TC = true
	t.RD = req.RD
	t.Rcode = resp.Rcode
	t.EDNSSize = req.EDNSSize
	out, _ := t.Pack(true)
	return out
}

// resolve builds the full response message for a parsed query.
func (s *Server) resolve(req *dnsmsg.Message, clientIP net.IP) (*dnsmsg.Message, string) {
	resp := &dnsmsg.Message{Questions: req.Questions}
	resp.ID = req.ID
	resp.QR = true
	resp.AA = true
	resp.RD = req.RD

	if len(req.Questions) == 0 {
		resp.Rcode = dnsmsg.RcodeFormErr
		return resp, ""
	}
	q := req.Questions[0]
	qname := strings.TrimSuffix(strings.ToLower(q.Name), ".")
	dc := ""

	z := s.Store.FindZone(qname)
	if z == nil {
		resp.Rcode = dnsmsg.RcodeRefused
		resp.AA = false
		return resp, ""
	}

	recs := z.RecordsAt(qname)
	// Also check for an exact-name miss: does any deeper name exist? If the
	// name itself has no records but is not empty-non-terminal relevant,
	// NXDOMAIN only when the name truly does not exist in the zone.
	_, nameExists := zoneHasName(z, qname)

	if !nameExists {
		resp.Rcode = dnsmsg.RcodeNXDomain
		resp.Authorities = append(resp.Authorities, z.SOARR())
		return resp, ""
	}

	switch q.Qtype {
	case dnsmsg.TypeA, dnsmsg.TypeAAAA:
		v6 := q.Qtype == dnsmsg.TypeAAAA
		// ALIAS at the apex: resolve the target ourselves and inline it.
		if al := findAlias(recs); al != nil {
			ips, ttl, err := s.Alias.ResolveAddrs(al.Value, v6)
			if err != nil {
				resp.Rcode = dnsmsg.RcodeServFail
				return resp, ""
			}
			for _, ip := range ips {
				resp.Answers = append(resp.Answers, dnsmsg.RR{
					Name: dnsmsg.Normalize(qname), Type: q.Qtype, Class: dnsmsg.ClassIN,
					TTL: ttlOr32(al.TTL, ttl), IP: ip,
				})
			}
			dc = "alias"
			return resp, dc
		}
		// CNAME at this name: return it and chase inside the zone.
		if cn := findType(recs, "CNAME"); cn != nil {
			rr, err := z.ToRR(*cn)
			if err == nil {
				resp.Answers = append(resp.Answers, rr)
				target := strings.TrimSuffix(strings.ToLower(cn.Value), ".")
				if s.Store.FindZone(target) == z {
					trecs := z.RecordsAt(target)
					sel, tdc := s.selectAddrs(z, trecs, q.Qtype, clientIP)
					dc = tdc
					resp.Answers = append(resp.Answers, sel...)
				}
			}
			return resp, dc
		}
		sel, sdc := s.selectAddrs(z, recs, q.Qtype, clientIP)
		dc = sdc
		if sel == nil {
			// Name exists but no usable A/AAAA: NODATA (or SERVFAIL when
			// endpoints exist but all are down and no fallback is set).
			if hasType(recs, q.Qtype) {
				resp.Rcode = dnsmsg.RcodeServFail
			} else {
				resp.Authorities = append(resp.Authorities, z.SOARR())
			}
			return resp, dc
		}
		resp.Answers = append(resp.Answers, sel...)
		return resp, dc

	case dnsmsg.TypeSOA:
		resp.Answers = append(resp.Answers, z.SOARR())
		return resp, ""

	case dnsmsg.TypeANY:
		for _, r := range recs {
			if r.Type == zone.TypeALIAS {
				continue
			}
			if rr, err := z.ToRR(r); err == nil {
				resp.Answers = append(resp.Answers, rr)
			}
		}
		if qname == z.Name {
			resp.Answers = append(resp.Answers, z.SOARR())
		}
		if len(resp.Answers) == 0 {
			resp.Authorities = append(resp.Authorities, z.SOARR())
		}
		return resp, ""

	default:
		tname := dnsmsg.TypeString(q.Qtype)
		matched := false
		for _, r := range recs {
			if r.Type != tname {
				continue
			}
			matched = true
			if rr, err := z.ToRR(r); err == nil {
				resp.Answers = append(resp.Answers, rr)
			}
		}
		if !matched {
			// NODATA: name exists, type does not.
			resp.Authorities = append(resp.Authorities, z.SOARR())
		}
		return resp, ""
	}
}

// selectAddrs picks the A/AAAA records to return for a name, applying
// health filtering, datacenter affinity and weighted split. Returns nil
// when nothing usable exists and no fallback is configured.
func (s *Server) selectAddrs(z *zone.Zone, recs []zone.Record, qtype uint16, clientIP net.IP) ([]dnsmsg.RR, string) {
	want := "A"
	if qtype == dnsmsg.TypeAAAA {
		want = "AAAA"
	}
	var cands []zone.Record
	for _, r := range recs {
		if r.Type == want {
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		return nil, ""
	}
	// Health filtering.
	var healthy []zone.Record
	for _, r := range cands {
		if s.Health == nil || s.Health.Healthy(r.Value) {
			healthy = append(healthy, r)
		}
	}
	if len(healthy) == 0 {
		// Everything down: serve the fallback address if configured.
		fb := z.Fallback4
		if want == "AAAA" {
			fb = z.Fallback6
		}
		if fb == "" {
			return nil, ""
		}
		rr, err := z.ToRR(zone.Record{Name: z.Name, Type: want, Value: fb})
		if err != nil {
			return nil, ""
		}
		rr.Name = dnsmsg.Normalize(recs[0].Name)
		return []dnsmsg.RR{rr}, "fallback"
	}

	// Group by datacenter.
	groups := map[string][]zone.Record{}
	order := []string{}
	for _, r := range healthy {
		d := r.DC
		if _, ok := groups[d]; !ok {
			order = append(order, d)
		}
		groups[d] = append(groups[d], r)
	}

	// Client subnet affinity.
	pick := ""
	if dc, _, ok := s.Geo.Lookup(clientIP); ok {
		if _, has := groups[dc]; has {
			pick = dc
		}
	}
	// Weighted split among groups when no affinity hit.
	if pick == "" && len(order) > 0 {
		total := 0
		ws := make([]int, len(order))
		for i, d := range order {
			// A configured datacenter weight wins; otherwise fall back to
			// the sum of the group's record weights (default 1 each).
			if w := s.DCWeights[d]; w > 0 {
				ws[i] = w
			} else {
				for _, r := range groups[d] {
					if r.Weight > 0 {
						ws[i] += r.Weight
					} else {
						ws[i]++
					}
				}
			}
			if ws[i] <= 0 {
				ws[i] = 1
			}
			total += ws[i]
		}
		x := rand.Intn(total)
		for i, w := range ws {
			if x < w {
				pick = order[i]
				break
			}
			x -= w
		}
		if pick == "" {
			pick = order[0]
		}
	}

	out := make([]dnsmsg.RR, 0, len(groups[pick]))
	for _, r := range groups[pick] {
		if rr, err := z.ToRR(r); err == nil {
			out = append(out, rr)
		}
	}
	return out, pick
}

func zoneHasName(z *zone.Zone, qname string) ([]zone.Record, bool) {
	_, ok := z.Records[qname]
	if ok {
		return z.Records[qname], true
	}
	if qname == z.Name {
		return nil, true // apex always exists (SOA)
	}
	return nil, false
}

func findAlias(recs []zone.Record) *zone.Record {
	for i := range recs {
		if recs[i].Type == zone.TypeALIAS {
			return &recs[i]
		}
	}
	return nil
}

func findType(recs []zone.Record, t string) *zone.Record {
	for i := range recs {
		if recs[i].Type == t {
			return &recs[i]
		}
	}
	return nil
}

func hasType(recs []zone.Record, qtype uint16) bool {
	want := dnsmsg.TypeString(qtype)
	for _, r := range recs {
		if r.Type == want {
			return true
		}
	}
	return false
}

func ttlOr32(v, def uint32) uint32 {
	if v == 0 {
		return def
	}
	return v
}

func rcodeName(c uint8) string {
	switch c {
	case dnsmsg.RcodeNoError:
		return "NOERROR"
	case dnsmsg.RcodeFormErr:
		return "FORMERR"
	case dnsmsg.RcodeServFail:
		return "SERVFAIL"
	case dnsmsg.RcodeNXDomain:
		return "NXDOMAIN"
	case dnsmsg.RcodeNotImp:
		return "NOTIMP"
	case dnsmsg.RcodeRefused:
		return "REFUSED"
	}
	return fmt.Sprintf("RCODE%d", c)
}

func (s *Server) log(start time.Time, clientIP net.IP, subnet, region string, req, resp *dnsmsg.Message, proto, rcode, dc string, dropped, slipped, truncated bool) {
	if s.Stats == nil {
		return
	}
	l := stats.QueryLog{
		Time: time.Now(), Client: clientIP.String(), Subnet: subnet, Region: region,
		RCode: rcode, DC: dc, Proto: proto,
		Truncated: truncated, Slipped: slipped, Dropped: dropped,
		ElapsedMs: float64(time.Since(start).Microseconds()) / 1000.0,
	}
	if len(req.Questions) > 0 {
		l.QName = req.Questions[0].Name
		l.QType = dnsmsg.TypeString(req.Questions[0].Qtype)
	}
	if resp != nil {
		var ans []string
		for _, rr := range resp.Answers {
			ans = append(ans, rr.String())
		}
		l.Answers = strings.Join(ans, " | ")
	}
	s.Stats.Log(l)
}

// ListenAndServe runs UDP and TCP listeners on addr until stop closes.
func (s *Server) ListenAndServe(addr string, stop <-chan struct{}) error {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	uc, err := net.ListenUDP("udp", ua)
	if err != nil {
		return err
	}
	ta, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return err
	}
	tl, err := net.ListenTCP("tcp", ta)
	if err != nil {
		uc.Close()
		return err
	}
	log.Printf("dns: listening on %s (udp+tcp)", addr)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.serveUDP(uc) }()
	go func() { defer wg.Done(); s.serveTCP(tl) }()

	<-stop
	uc.Close()
	tl.Close()
	wg.Wait()
	return nil
}

func (s *Server) serveUDP(conn *net.UDPConn) {
	buf := make([]byte, 65535)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		wire := append([]byte(nil), buf[:n]...)
		go func() {
			out := s.ServeDNS(wire, addr.IP, "udp")
			if out != nil {
				conn.WriteToUDP(out, addr)
			}
		}()
	}
}

func (s *Server) serveTCP(l *net.TCPListener) {
	for {
		conn, err := l.AcceptTCP()
		if err != nil {
			return
		}
		go s.handleTCPConn(conn)
	}
}

func (s *Server) handleTCPConn(conn *net.TCPConn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	var ln [2]byte
	for {
		if _, err := io.ReadFull(conn, ln[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(ln[:]))
		if n == 0 || n > 65535 {
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
		if err != nil {
			return
		}
		out := s.ServeDNS(buf, net.ParseIP(host), "tcp")
		if out == nil {
			continue
		}
		binary.BigEndian.PutUint16(ln[:], uint16(len(out)))
		if _, err := conn.Write(append(ln[:], out...)); err != nil {
			return
		}
	}
}
