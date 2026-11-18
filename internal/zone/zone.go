// Package zone holds the authoritative zone data model and an
// in-memory, concurrency-safe store.
package zone

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dnsd/internal/dnsmsg"
)

// TypeALIAS is the pseudo-type for apex alias records. It is stored in
// zone data but never emitted on the wire; the server resolves the target
// itself and inlines the resulting addresses.
const TypeALIAS = "ALIAS"

// Record is one zone record in presentation form.
type Record struct {
	Name   string `json:"name"`             // FQDN without trailing dot, lowercase
	Type   string `json:"type"`             // A AAAA CNAME MX TXT SRV NS ALIAS
	TTL    uint32 `json:"ttl"`              // 0 = use zone default
	Value  string `json:"value"`            // presentation-format rdata
	DC     string `json:"dc,omitempty"`     // datacenter tag for A/AAAA (geo routing)
	Weight int    `json:"weight,omitempty"` // traffic-split weight for A/AAAA
}

// SOA holds the zone's SOA parameters.
type SOA struct {
	MName   string `json:"mname"`
	RName   string `json:"rname"`
	Serial  uint32 `json:"serial"`
	Refresh uint32 `json:"refresh"`
	Retry   uint32 `json:"retry"`
	Expire  uint32 `json:"expire"`
	Minimum uint32 `json:"minimum"`
	TTL     uint32 `json:"ttl"`
}

// HealthCfg configures active health checking of A/AAAA endpoints in a zone.
type HealthCfg struct {
	Enabled  bool   `json:"enabled"`
	Type     string `json:"type"` // "tcp" or "http"
	Port     int    `json:"port"`
	Path     string `json:"path"` // http only
	Interval int    `json:"interval_sec"`
	Timeout  int    `json:"timeout_sec"`
}

// Zone is one authoritative zone.
type Zone struct {
	Name       string              `json:"name"` // apex, no trailing dot
	DefaultTTL uint32              `json:"default_ttl"`
	SOA        SOA                 `json:"soa"`
	Fallback4  string              `json:"fallback4"` // last-resort A when all endpoints down
	Fallback6  string              `json:"fallback6"` // last-resort AAAA
	Health     HealthCfg           `json:"health"`
	Records    map[string][]Record `json:"records"` // key: owner name (fqdn, no dot)
}

// NewZone creates a zone with a sane default SOA.
func NewZone(name string) *Zone {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	return &Zone{
		Name:       name,
		DefaultTTL: 300,
		SOA: SOA{
			MName:   "ns1." + name + ".",
			RName:   "hostmaster." + name + ".",
			Serial:  uint32(time.Now().Unix()),
			Refresh: 7200, Retry: 3600, Expire: 1209600, Minimum: 60,
			TTL: 300,
		},
		Health:  HealthCfg{Type: "tcp", Port: 80, Interval: 5, Timeout: 2},
		Records: map[string][]Record{},
	}
}

// Touch bumps the SOA serial after a data change.
func (z *Zone) Touch() { z.SOA.Serial = uint32(time.Now().Unix()) }

// SOARR renders the zone SOA as a wire RR.
func (z *Zone) SOARR() dnsmsg.RR {
	return dnsmsg.RR{
		Name: z.Name + ".", Type: dnsmsg.TypeSOA, Class: dnsmsg.ClassIN, TTL: z.SOA.TTL,
		Target: z.SOA.MName, Rname: z.SOA.RName,
		Serial: z.SOA.Serial, Refresh: z.SOA.Refresh, Retry: z.SOA.Retry,
		Expire: z.SOA.Expire, Minimum: z.SOA.Minimum,
	}
}

// ToRR converts a stored record to a wire RR.
func (z *Zone) ToRR(r Record) (dnsmsg.RR, error) {
	ttl := r.TTL
	if ttl == 0 {
		ttl = z.DefaultTTL
	}
	rr := dnsmsg.RR{Name: dnsmsg.Normalize(r.Name), Class: dnsmsg.ClassIN, TTL: ttl}
	code, ok := dnsmsg.NameToType[r.Type]
	if !ok {
		return rr, fmt.Errorf("unsupported type %q", r.Type)
	}
	rr.Type = code
	v := strings.TrimSpace(r.Value)
	switch r.Type {
	case "A":
		ip := net.ParseIP(v)
		if ip == nil || ip.To4() == nil {
			return rr, fmt.Errorf("bad A value %q", v)
		}
		rr.IP = ip.To4()
	case "AAAA":
		ip := net.ParseIP(v)
		if ip == nil || ip.To4() != nil {
			return rr, fmt.Errorf("bad AAAA value %q", v)
		}
		rr.IP = ip
	case "CNAME", "NS":
		rr.Target = dnsmsg.Normalize(v)
	case "MX":
		f := strings.Fields(v)
		if len(f) != 2 {
			return rr, fmt.Errorf("MX value must be \"<pref> <host>\"")
		}
		p, err := strconv.Atoi(f[0])
		if err != nil {
			return rr, fmt.Errorf("bad MX preference: %v", err)
		}
		rr.Preference = uint16(p)
		rr.Target = dnsmsg.Normalize(f[1])
	case "TXT":
		rr.Text = []string{strings.Trim(v, `"`)}
	case "SRV":
		f := strings.Fields(v)
		if len(f) != 4 {
			return rr, fmt.Errorf("SRV value must be \"<prio> <weight> <port> <host>\"")
		}
		n := make([]int, 3)
		for i := 0; i < 3; i++ {
			x, err := strconv.Atoi(f[i])
			if err != nil {
				return rr, fmt.Errorf("bad SRV field: %v", err)
			}
			n[i] = x
		}
		rr.Priority, rr.Weight, rr.Port = uint16(n[0]), uint16(n[1]), uint16(n[2])
		rr.Target = dnsmsg.Normalize(f[3])
	default:
		return rr, fmt.Errorf("unsupported type %q", r.Type)
	}
	return rr, nil
}

// Store is a concurrency-safe collection of zones.
type Store struct {
	mu       sync.RWMutex
	zones    map[string]*Zone
	onChange func(z *Zone) // persistence hook, called with lock released
}

// NewStore creates an empty store. onChange may be nil.
func NewStore(onChange func(z *Zone)) *Store {
	return &Store{zones: map[string]*Zone{}, onChange: onChange}
}

func (s *Store) changed(z *Zone) {
	if s.onChange != nil {
		s.onChange(z)
	}
}

// AddZone inserts or replaces a zone.
func (s *Store) AddZone(z *Zone) {
	s.mu.Lock()
	s.zones[z.Name] = z
	s.mu.Unlock()
	s.changed(z)
}

// RemoveZone deletes a zone.
func (s *Store) RemoveZone(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	s.mu.Lock()
	_, ok := s.zones[name]
	delete(s.zones, name)
	s.mu.Unlock()
	return ok
}

// Get returns the zone for an exact apex name.
func (s *Store) Get(name string) *Zone {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.zones[name]
}

// List returns all zones sorted by name.
func (s *Store) List() []*Zone {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Zone, 0, len(s.zones))
	for _, z := range s.zones {
		out = append(out, z)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FindZone returns the zone with the longest apex suffix matching qname.
func (s *Store) FindZone(qname string) *Zone {
	qname = strings.TrimSuffix(strings.ToLower(qname), ".")
	s.mu.RLock()
	defer s.mu.RUnlock()
	for n := qname; ; {
		if z, ok := s.zones[n]; ok {
			return z
		}
		i := strings.IndexByte(n, '.')
		if i < 0 {
			return nil
		}
		n = n[i+1:]
	}
}

// ErrNotFound is returned when a record or name does not exist.
var ErrNotFound = errors.New("record not found")

// AddRecord appends a record to a zone.
func (s *Store) AddRecord(zoneName string, r Record) error {
	z := s.Get(zoneName)
	if z == nil {
		return fmt.Errorf("zone %q not found", zoneName)
	}
	r.Name = strings.TrimSuffix(strings.ToLower(r.Name), ".")
	if r.Name == "" || r.Name == "@" {
		r.Name = z.Name
	}
	if !strings.HasSuffix(r.Name, z.Name) {
		return fmt.Errorf("record name %q outside zone %q", r.Name, z.Name)
	}
	r.Type = strings.ToUpper(r.Type)
	if r.Type != TypeALIAS {
		if _, err := z.ToRR(r); err != nil {
			return err
		}
	} else if r.Name != z.Name {
		return fmt.Errorf("ALIAS is only allowed at the zone apex")
	}
	s.mu.Lock()
	z.Records[r.Name] = append(z.Records[r.Name], r)
	z.Touch()
	s.mu.Unlock()
	s.changed(z)
	return nil
}

// DeleteRecord removes the idx-th record of the given type at name.
func (s *Store) DeleteRecord(zoneName, name, rtype string, idx int) error {
	z := s.Get(zoneName)
	if z == nil {
		return fmt.Errorf("zone %q not found", zoneName)
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	rtype = strings.ToUpper(rtype)
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := z.Records[name]
	out := recs[:0]
	seen := -1
	for _, r := range recs {
		if r.Type == rtype {
			seen++
			if seen == idx {
				continue
			}
		}
		out = append(out, r)
	}
	if len(out) == len(recs) {
		return ErrNotFound
	}
	if len(out) == 0 {
		delete(z.Records, name)
	} else {
		z.Records[name] = out
	}
	z.Touch()
	go s.changed(z)
	return nil
}

// UpdateRecord replaces the idx-th record of the given type at name.
func (s *Store) UpdateRecord(zoneName, name, rtype string, idx int, nr Record) error {
	z := s.Get(zoneName)
	if z == nil {
		return fmt.Errorf("zone %q not found", zoneName)
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	rtype = strings.ToUpper(rtype)
	nr.Type = rtype
	nr.Name = name
	if nr.Type != TypeALIAS {
		if _, err := z.ToRR(nr); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := z.Records[name]
	seen := -1
	for i, r := range recs {
		if r.Type == rtype {
			seen++
			if seen == idx {
				recs[i] = nr
				z.Touch()
				go s.changed(z)
				return nil
			}
		}
	}
	return ErrNotFound
}

// RecordsAt returns a copy of the records at name.
func (z *Zone) RecordsAt(name string) []Record {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	recs := z.Records[name]
	out := make([]Record, len(recs))
	copy(out, recs)
	return out
}
