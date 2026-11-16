// Package zone holds authoritative zone data and implements the lookup
// logic: exact match, wildcards, CNAME chains and negative answers.
package zone

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	"gyndns/internal/dnsmsg"
)

// MaxCNAMEChain bounds CNAME chasing; longer chains are treated as errors.
const MaxCNAMEChain = 8

// Record is one zone record with routing/health metadata.
type Record struct {
	Name    string   `json:"name"` // owner name, fqdn, lower case
	Type    uint16   `json:"type"`
	TTL     uint32   `json:"ttl"`
	RData   string   `json:"rdata"`             // presentation format
	Weight  int      `json:"weight,omitempty"`  // traffic weight, 0 means default(100)
	Subnets []string `json:"subnets,omitempty"` // client CIDRs this record serves; empty = any
	Backup  bool     `json:"backup,omitempty"`  // only used when everything else is down
	Check   string   `json:"check,omitempty"`   // health check: tcp://host:port or http://host:port/path
}

// Key uniquely identifies the record for health status tracking.
func (r *Record) Key() string {
	return fmt.Sprintf("%s|%s|%s", r.Name, dnsmsg.TypeString(r.Type), r.RData)
}

// RR converts to a wire record. owner overrides the record name (wildcard use).
func (r *Record) RR(owner string) (dnsmsg.RR, error) {
	data, err := dnsmsg.ParseRDataString(r.Type, r.RData)
	if err != nil {
		return dnsmsg.RR{}, err
	}
	if owner == "" {
		owner = r.Name
	}
	return dnsmsg.RR{Name: owner, Type: r.Type, Class: dnsmsg.ClassIN, TTL: r.TTL, Data: data}, nil
}

// EffectiveWeight returns the traffic weight with the default applied.
func (r *Record) EffectiveWeight() int {
	if r.Weight <= 0 {
		return 100
	}
	return r.Weight
}

// Zone is one authoritative zone.
type Zone struct {
	Name    string    `json:"name"` // apex, fqdn lower case
	Records []*Record `json:"records"`

	index  map[string][]*Record // owner name -> records
	exists map[string]bool      // all existing names incl. empty non-terminals
}

// Index (re)builds the owner-name index. Called after any mutation.
func (z *Zone) Index() {
	z.index = make(map[string][]*Record, len(z.Records))
	z.exists = make(map[string]bool)
	for _, r := range z.Records {
		z.index[r.Name] = append(z.index[r.Name], r)
	}
	// Mark every ancestor of every owner name as existing (empty
	// non-terminals included) so wildcard closest-encloser matching
	// follows RFC 4592.
	for name := range z.index {
		z.exists[name] = true
		labels := strings.Split(strings.TrimSuffix(name, "."), ".")
		for i := 1; i < len(labels); i++ {
			anc := dnsmsg.Fqdn(strings.Join(labels[i:], "."))
			z.exists[anc] = true
			if anc == z.Name {
				break
			}
		}
	}
}

func (z *Zone) indexed() {
	if z.index == nil {
		z.Index()
	}
}

// SOA returns the zone's SOA record.
func (z *Zone) SOA() *Record {
	z.indexed()
	for _, r := range z.index[z.Name] {
		if r.Type == dnsmsg.TypeSOA {
			return r
		}
	}
	return nil
}

// Serial returns the zone serial from the SOA record (0 if missing).
func (z *Zone) Serial() uint32 {
	soa := z.SOA()
	if soa == nil {
		return 0
	}
	d, err := dnsmsg.ParseRDataString(dnsmsg.TypeSOA, soa.RData)
	if err != nil {
		return 0
	}
	return d.(*dnsmsg.SOA).Serial
}

// BumpSerial increments the SOA serial in place.
func (z *Zone) BumpSerial() {
	soa := z.SOA()
	if soa == nil {
		return
	}
	d, err := dnsmsg.ParseRDataString(dnsmsg.TypeSOA, soa.RData)
	if err != nil {
		return
	}
	s := d.(*dnsmsg.SOA)
	s.Serial++
	soa.RData = s.String()
}

// match finds records for name: exact match first, then wildcard per
// RFC 4592 (closest encloser, empty non-terminals count as existing).
// Returns the records (possibly empty for an existing name with no
// records) and the owner name to use in answers (the query name itself
// for wildcard matches).
func (z *Zone) match(name string) (recs []*Record, owner string, found bool) {
	z.indexed()
	if r, ok := z.index[name]; ok {
		return r, name, true
	}
	if z.exists[name] { // empty non-terminal
		return nil, name, true
	}
	// Wildcard: locate the closest encloser, then look for "*."+encloser.
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i := 1; i < len(labels); i++ {
		anc := dnsmsg.Fqdn(strings.Join(labels[i:], "."))
		if z.exists[anc] {
			if wr, ok := z.index["*."+anc]; ok {
				return wr, name, true
			}
			return nil, "", false
		}
		if anc == z.Name {
			return nil, "", false
		}
	}
	return nil, "", false
}

// Change is one journal entry for incremental zone transfer.
type Change struct {
	Serial uint32   `json:"serial"`
	Add    []Record `json:"add,omitempty"`
	Del    []Record `json:"del,omitempty"`
}

// Selector picks which of the candidate records actually get answered
// (subnet match, weights, health). Implemented by the router package.
type Selector interface {
	Select(cands []*Record, client net.IP, trace func(format string, args ...any)) []*Record
}

// Result is the outcome of resolving one query.
type Result struct {
	RCode       uint8
	Answers     []dnsmsg.RR
	Authorities []dnsmsg.RR
	Trace       []string
	Zone        string
}

// Store keeps all zones, the change journals and persistence.
type Store struct {
	mu       sync.RWMutex
	zones    map[string]*Zone
	journals map[string][]Change
	Selector Selector
	// OnChange fires after any master-side mutation (used to send NOTIFY).
	OnChange func(zone string, serial uint32)
	// ReadOnly rejects API mutations (slave mode).
	ReadOnly bool

	maxJournal  int
	persistPath string
}

// NewStore creates an empty store.
func NewStore() *Store {
	return &Store{
		zones:      make(map[string]*Zone),
		journals:   make(map[string][]Change),
		maxJournal: 200,
	}
}

// Normalize lower-cases and fqdns a name.
func Normalize(name string) string {
	return strings.ToLower(dnsmsg.Fqdn(strings.TrimSpace(name)))
}

// GetZone returns a zone by name (any casing, dot optional).
func (s *Store) GetZone(name string) *Zone {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.zones[Normalize(name)]
}

// Zones returns all zone names, sorted.
func (s *Store) Zones() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.zones))
	for n := range s.zones {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Journal returns the recorded changes for a zone.
func (s *Store) Journal(zone string) []Change {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Change, len(s.journals[Normalize(zone)]))
	copy(out, s.journals[Normalize(zone)])
	return out
}

// Resolve answers a query against the authoritative data.
func (s *Store) Resolve(qname string, qtype uint16, client net.IP) *Result {
	res := &Result{}
	trace := func(format string, args ...any) {
		res.Trace = append(res.Trace, fmt.Sprintf(format, args...))
	}
	qname = Normalize(qname)

	s.mu.RLock()
	var z *Zone
	for zn, zone := range s.zones {
		if qname == zn || strings.HasSuffix(qname, "."+zn) {
			if z == nil || len(zn) > len(z.Name) {
				z = zone
			}
		}
	}
	s.mu.RUnlock()
	if z == nil {
		res.RCode = dnsmsg.RcodeRefused
		trace("域名 %s 不在本服务器负责的区内, 拒绝应答", qname)
		return res
	}
	res.Zone = z.Name
	trace("命中区 %s", z.Name)

	sel := s.Selector
	visited := map[string]bool{}
	cur := qname
	for depth := 0; depth < MaxCNAMEChain; depth++ {
		if visited[cur] {
			res.RCode = dnsmsg.RcodeServerFailure
			trace("CNAME 成环: %s 再次出现, 返回 SERVFAIL", cur)
			return res
		}
		visited[cur] = true

		recs, owner, found := z.match(cur)
		if !found {
			res.RCode = dnsmsg.RcodeNameError
			trace("区内无 %s (含通配符) , 返回 NXDOMAIN", cur)
			res.Authorities = s.soaAuthority(z)
			return res
		}
		if owner != cur {
			trace("%s 命中通配符记录", cur)
		}

		var typed, cnames []*Record
		for _, r := range recs {
			switch {
			case r.Type == qtype || qtype == dnsmsg.TypeANY:
				typed = append(typed, r)
			case r.Type == dnsmsg.TypeCNAME:
				cnames = append(cnames, r)
			}
		}
		if len(typed) > 0 {
			if (qtype == dnsmsg.TypeA || qtype == dnsmsg.TypeAAAA) && sel != nil {
				trace("候选地址 %d 条, 进入选路(子网/权重/健康检查)", len(typed))
				typed = sel.Select(typed, client, trace)
			}
			for _, r := range typed {
				rr, err := r.RR(owner)
				if err != nil {
					trace("记录 %s 解析失败: %v", r.Key(), err)
					continue
				}
				res.Answers = append(res.Answers, rr)
			}
			res.RCode = dnsmsg.RcodeSuccess
			trace("应答 %d 条 %s 记录", len(res.Answers), dnsmsg.TypeString(qtype))
			return res
		}
		if len(cnames) > 0 && qtype != dnsmsg.TypeCNAME {
			rr, err := cnames[0].RR(owner)
			if err == nil {
				res.Answers = append(res.Answers, rr)
			}
			target := Normalize(cnames[0].RData)
			trace("%s 是 CNAME, 指向 %s", cur, target)
			if !(target == z.Name || strings.HasSuffix(target, "."+z.Name)) {
				res.RCode = dnsmsg.RcodeSuccess
				trace("目标 %s 不在本区, 作为权威服务器不再递归, 直接返回", target)
				return res
			}
			cur = target
			continue
		}
		// Name exists but no record of the requested type: NODATA.
		res.RCode = dnsmsg.RcodeSuccess
		trace("%s 存在但无 %s 记录, 返回空应答+SOA (NODATA)", cur, dnsmsg.TypeString(qtype))
		res.Authorities = s.soaAuthority(z)
		return res
	}
	res.RCode = dnsmsg.RcodeServerFailure
	trace("CNAME 链超过 %d 层, 返回 SERVFAIL", MaxCNAMEChain)
	return res
}

func (s *Store) soaAuthority(z *Zone) []dnsmsg.RR {
	soa := z.SOA()
	if soa == nil {
		return nil
	}
	rr, err := soa.RR("")
	if err != nil {
		return nil
	}
	if d, ok := rr.Data.(*dnsmsg.SOA); ok && d.MinTTL < rr.TTL {
		rr.TTL = d.MinTTL
	}
	return []dnsmsg.RR{rr}
}
