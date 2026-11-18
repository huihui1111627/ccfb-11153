// Package ratelimit implements per-source-subnet response rate limiting
// with an identical-response (anti reflection/amplification) policy.
package ratelimit

import (
	"sync"
	"time"
)

// Policy configures the limiter.
type Policy struct {
	Enabled         bool    `json:"enabled"`
	PrefixV4        int     `json:"prefix_v4"`         // subnet mask for grouping clients, default 24
	PrefixV6        int     `json:"prefix_v6"`         // default 56
	QPS             float64 `json:"qps"`               // sustained responses per second per subnet
	Burst           int     `json:"burst"`             // token bucket size
	IdenticalMax    int     `json:"identical_max"`     // max identical responses per window per subnet
	IdenticalWindow int     `json:"identical_window"`  // window in seconds
	Action          string  `json:"action"`            // "slip" (TC=1 empty) or "drop"
}

// DefaultPolicy returns sane anti-amplification defaults.
func DefaultPolicy() Policy {
	return Policy{
		Enabled: true, PrefixV4: 24, PrefixV6: 56,
		QPS: 20, Burst: 40,
		IdenticalMax: 5, IdenticalWindow: 2,
		Action: "slip",
	}
}

// Verdict is what to do with a response.
type Verdict int

const (
	Allow Verdict = iota
	Slip           // send truncated empty response (TC=1)
	Drop           // send nothing
)

type bucket struct {
	tokens float64
	last   time.Time
}

type identRec struct {
	key     uint64
	count   int
	expires time.Time
}

// Limiter tracks per-subnet token buckets and identical-response counts.
type Limiter struct {
	mu      sync.Mutex
	policy  Policy
	buckets map[string]*bucket
	ident   map[string]*identRec
	now     func() time.Time
}

// New creates a limiter with the given policy.
func New(p Policy) *Limiter {
	if p.PrefixV4 == 0 {
		p.PrefixV4 = 24
	}
	if p.PrefixV6 == 0 {
		p.PrefixV6 = 56
	}
	if p.IdenticalWindow == 0 {
		p.IdenticalWindow = 2
	}
	if p.Action == "" {
		p.Action = "slip"
	}
	return &Limiter{
		policy:  p,
		buckets: map[string]*bucket{},
		ident:   map[string]*identRec{},
		now:     time.Now,
	}
}

// Policy returns the current policy.
func (l *Limiter) Policy() Policy {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.policy
}

// SetPolicy replaces the policy at runtime.
func (l *Limiter) SetPolicy(p Policy) {
	l.mu.Lock()
	l.policy = p
	l.mu.Unlock()
}

// CheckSubnet consumes one token for the subnet; returns false when the
// subnet is over its rate.
func (l *Limiter) CheckSubnet(subnet string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.policy
	if !p.Enabled || p.QPS <= 0 {
		return true
	}
	now := l.now()
	b, ok := l.buckets[subnet]
	if !ok {
		b = &bucket{tokens: float64(p.Burst), last: now}
		l.buckets[subnet] = b
	}
	el := now.Sub(b.last).Seconds()
	b.tokens += el * p.QPS
	if b.tokens > float64(p.Burst) {
		b.tokens = float64(p.Burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// CheckIdentical records a response about to be sent and returns the
// verdict for repeats of the same response to the same subnet.
// respKey should identify (qname, qtype, answer-set).
func (l *Limiter) CheckIdentical(subnet string, respKey uint64) Verdict {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.policy
	if !p.Enabled || p.IdenticalMax <= 0 {
		return Allow
	}
	now := l.now()
	k := subnet
	r, ok := l.ident[k]
	if !ok || r.key != respKey || now.After(r.expires) {
		l.ident[k] = &identRec{key: respKey, count: 1, expires: now.Add(time.Duration(p.IdenticalWindow) * time.Second)}
		return Allow
	}
	r.count++
	if r.count <= p.IdenticalMax {
		return Allow
	}
	if p.Action == "drop" {
		return Drop
	}
	return Slip
}

// GC periodically removes stale entries; run in a goroutine.
func (l *Limiter) GC(stop <-chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			l.mu.Lock()
			for k, b := range l.buckets {
				if now.Sub(b.last) > 10*time.Minute {
					delete(l.buckets, k)
				}
			}
			for k, r := range l.ident {
				if now.After(r.expires) {
					delete(l.ident, k)
				}
			}
			l.mu.Unlock()
		}
	}
}
