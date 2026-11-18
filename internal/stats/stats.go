// Package stats records query logs and per-subnet datacenter assignment
// counters for the management UI.
package stats

import (
	"sort"
	"sync"
	"time"
)

// QueryLog is one answered (or dropped) query.
type QueryLog struct {
	Time      time.Time `json:"time"`
	Client    string    `json:"client"`
	Subnet    string    `json:"subnet"`
	Region    string    `json:"region"`
	QName     string    `json:"qname"`
	QType     string    `json:"qtype"`
	RCode     string    `json:"rcode"`
	DC        string    `json:"dc"`
	Answers   string    `json:"answers"`
	Proto     string    `json:"proto"` // udp / tcp
	Truncated bool      `json:"truncated"`
	Slipped   bool      `json:"slipped"`
	Dropped   bool      `json:"dropped"`
	ElapsedMs float64   `json:"elapsed_ms"`
}

// Assignment counts how often a subnet was routed to each datacenter.
type Assignment struct {
	Subnet string         `json:"subnet"`
	Region string         `json:"region"`
	Total  int64          `json:"total"`
	ByDC   map[string]int64 `json:"by_dc"`
}

// RegionStat aggregates assignments per region and datacenter.
type RegionStat struct {
	Region string         `json:"region"`
	Total  int64          `json:"total"`
	ByDC   map[string]int64 `json:"by_dc"`
}

// Stats is a concurrency-safe ring of query logs plus counters.
type Stats struct {
	mu     sync.Mutex
	logs   []QueryLog
	head   int
	count  int
	cap    int
	assign map[string]*Assignment // key: subnet
}

// New creates a Stats with the given log ring capacity.
func New(capacity int) *Stats {
	if capacity <= 0 {
		capacity = 2000
	}
	return &Stats{cap: capacity, assign: map[string]*Assignment{}}
}

// Log records a query.
func (s *Stats) Log(l QueryLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.logs) < s.cap {
		s.logs = append(s.logs, l)
	} else {
		s.logs[s.head] = l
		s.head = (s.head + 1) % s.cap
	}
	s.count++
	if l.DC != "" && !l.Dropped {
		a, ok := s.assign[l.Subnet]
		if !ok {
			a = &Assignment{Subnet: l.Subnet, Region: l.Region, ByDC: map[string]int64{}}
			s.assign[l.Subnet] = a
		}
		a.ByDC[l.DC]++
		a.Total++
	}
}

// Logs returns the most recent n logs, newest first.
func (s *Stats) Logs(n int) []QueryLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n > len(s.logs) {
		n = len(s.logs)
	}
	out := make([]QueryLog, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s.logs[len(s.logs)-1-i])
	}
	return out
}

// Total returns the number of queries seen since start.
func (s *Stats) Total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// Assignments returns per-subnet datacenter assignment counters.
func (s *Stats) Assignments() []Assignment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Assignment, 0, len(s.assign))
	for _, a := range s.assign {
		cp := *a
		cp.ByDC = map[string]int64{}
		for k, v := range a.ByDC {
			cp.ByDC[k] = v
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	return out
}

// ByRegion aggregates assignments per region.
func (s *Stats) ByRegion() []RegionStat {
	m := map[string]*RegionStat{}
	for _, a := range s.Assignments() {
		r, ok := m[a.Region]
		if !ok {
			r = &RegionStat{Region: a.Region, ByDC: map[string]int64{}}
			m[a.Region] = r
		}
		r.Total += a.Total
		for dc, n := range a.ByDC {
			r.ByDC[dc] += n
		}
	}
	out := make([]RegionStat, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	return out
}
