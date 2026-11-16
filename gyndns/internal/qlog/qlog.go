// Package qlog keeps a ring buffer of recent DNS answers with the
// decision trace of how each answer was selected.
package qlog

import (
	"strings"
	"sync"
	"time"
)

// Entry is one answered query.
type Entry struct {
	Time      time.Time `json:"time"`
	Client    string    `json:"client"`
	Proto     string    `json:"proto"` // udp / tcp
	QName     string    `json:"qname"`
	QType     string    `json:"qtype"`
	RCode     string    `json:"rcode"`
	Zone      string    `json:"zone,omitempty"`
	Answers   []string  `json:"answers"`
	Trace     []string  `json:"trace"`
	Latency   float64   `json:"latency_ms"`
	Truncated bool      `json:"truncated,omitempty"`
}

// Log is a fixed-size ring buffer of entries.
type Log struct {
	mu   sync.RWMutex
	buf  []Entry
	head int
	full bool
	max  int
}

// New creates a log holding at most max entries.
func New(max int) *Log {
	return &Log{max: max, buf: make([]Entry, max)}
}

// Add appends an entry.
func (l *Log) Add(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf[l.head] = e
	l.head = (l.head + 1) % l.max
	if l.head == 0 {
		l.full = true
	}
}

// Query returns matching entries, newest first. Empty filters match all.
func (l *Log) Query(qname, client string, limit int) []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := l.head
	if l.full {
		n = l.max
	}
	var out []Entry
	for i := 0; i < n; i++ {
		idx := (l.head - 1 - i + l.max) % l.max
		e := l.buf[idx]
		if e.Time.IsZero() {
			continue
		}
		if qname != "" && !strings.Contains(e.QName, qname) {
			continue
		}
		if client != "" && !strings.Contains(e.Client, client) {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
