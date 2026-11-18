// Package alias resolves ALIAS target hostnames through upstream
// recursors and caches the results, so apex aliases can be inlined
// into answers as A/AAAA records.
package alias

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"dnsd/internal/dnsmsg"
)

type cacheEntry struct {
	ips     []net.IP
	expires time.Time
}

// Resolver resolves names through upstream recursive servers.
type Resolver struct {
	upstreams []string
	timeout   time.Duration

	mu    sync.Mutex
	cache map[string]*cacheEntry // key: "name|type"
}

// New creates a resolver using the given upstreams ("host:port").
func New(upstreams []string, timeout time.Duration) *Resolver {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Resolver{upstreams: upstreams, timeout: timeout, cache: map[string]*cacheEntry{}}
}

// ResolveAddrs returns cached-or-fresh A (v6=false) or AAAA (v6=true)
// addresses for name, along with the remaining TTL to advertise.
func (r *Resolver) ResolveAddrs(name string, v6 bool) ([]net.IP, uint32, error) {
	qtype := dnsmsg.TypeA
	if v6 {
		qtype = dnsmsg.TypeAAAA
	}
	key := fmt.Sprintf("%s|%d", dnsmsg.Normalize(name), qtype)

	r.mu.Lock()
	if e, ok := r.cache[key]; ok && time.Now().Before(e.expires) {
		ttl := uint32(time.Until(e.expires).Seconds())
		ips := append([]net.IP(nil), e.ips...)
		r.mu.Unlock()
		return ips, ttl, nil
	}
	r.mu.Unlock()

	msg, err := r.query(name, qtype)
	if err != nil {
		return nil, 0, err
	}
	var ips []net.IP
	var ttl uint32 = 60
	first := true
	for _, rr := range msg.Answers {
		if rr.Type == qtype && rr.IP != nil {
			ips = append(ips, rr.IP)
			if first || rr.TTL < ttl {
				ttl = rr.TTL
				first = false
			}
		}
	}
	if len(ips) == 0 {
		return nil, 0, fmt.Errorf("no %s records for %s", dnsmsg.TypeString(qtype), name)
	}
	if ttl > 300 {
		ttl = 300 // cap cache time so alias targets can fail over
	}
	if ttl == 0 {
		ttl = 1
	}
	r.mu.Lock()
	r.cache[key] = &cacheEntry{ips: ips, expires: time.Now().Add(time.Duration(ttl) * time.Second)}
	r.mu.Unlock()
	return ips, ttl, nil
}

// query sends one question to the upstreams, UDP first, TCP on truncation.
func (r *Resolver) query(name string, qtype uint16) (*dnsmsg.Message, error) {
	req := &dnsmsg.Message{
		Header:    Header(uint16(rand.Uint32())),
		Questions: []dnsmsg.Question{{Name: dnsmsg.Normalize(name), Qtype: qtype, Qclass: dnsmsg.ClassIN}},
	}
	req.RD = true
	req.EDNSSize = 1232
	wire, err := req.Pack(true)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, up := range r.upstreams {
		resp, err := r.exchange(up, wire, req.ID)
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no upstreams configured")
	}
	return nil, lastErr
}

// Header builds a request header with the given ID.
func Header(id uint16) dnsmsg.Header {
	return dnsmsg.Header{ID: id, RD: true}
}

func (r *Resolver) exchange(up string, wire []byte, id uint16) (*dnsmsg.Message, error) {
	conn, err := net.DialTimeout("udp", up, r.timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(r.timeout))
	if _, err := conn.Write(wire); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	msg, err := dnsmsg.Unpack(buf[:n])
	if err != nil {
		return nil, err
	}
	if msg.ID != id {
		return nil, fmt.Errorf("upstream %s: id mismatch", up)
	}
	if !msg.TC {
		return msg, nil
	}
	// Truncated: retry over TCP.
	tc, err := net.DialTimeout("tcp", up, r.timeout)
	if err != nil {
		return nil, err
	}
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(r.timeout))
	var ln [2]byte
	binary.BigEndian.PutUint16(ln[:], uint16(len(wire)))
	if _, err := tc.Write(append(ln[:], wire...)); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(tc, ln[:]); err != nil {
		return nil, err
	}
	rl := int(binary.BigEndian.Uint16(ln[:]))
	rbuf := make([]byte, rl)
	if _, err := io.ReadFull(tc, rbuf); err != nil {
		return nil, err
	}
	return dnsmsg.Unpack(rbuf)
}
