// Package health runs periodic checks against record targets and tracks
// which ones are up. Records whose check fails are removed from answers.
package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Target is one thing to check.
type Target struct {
	RecKey string // zone.Record.Key()
	Check  string // tcp://host:port or http(s)://host:port/path
}

// Status is the observed state of one target.
type Status struct {
	RecKey    string    `json:"rec_key"`
	Check     string    `json:"check"`
	Up        bool      `json:"up"`
	Forced    *bool     `json:"forced,omitempty"` // manual override, if set
	Failures  int       `json:"failures"`
	LastCheck time.Time `json:"last_check"`
	LastError string    `json:"last_error,omitempty"`
}

// Checker polls targets and remembers their state.
type Checker struct {
	mu       sync.RWMutex
	status   map[string]*Status // by RecKey
	interval time.Duration
	timeout  time.Duration
	// FailThreshold consecutive failures before a target is marked down.
	FailThreshold int

	client *http.Client
	cancel context.CancelFunc
}

// New creates a checker with the given poll interval.
func New(interval time.Duration) *Checker {
	return &Checker{
		status:        make(map[string]*Status),
		interval:      interval,
		timeout:       2 * time.Second,
		FailThreshold: 2,
		client:        &http.Client{Timeout: 2 * time.Second},
	}
}

// Up implements router.Health. A nil checker reports everything up.
func (c *Checker) Up(recKey string) bool {
	if c == nil {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	st, ok := c.status[recKey]
	if !ok {
		return true // no data yet: optimistic
	}
	if st.Forced != nil {
		return *st.Forced
	}
	return st.Up
}

// SetOverride forces a target up (true) or down (false); nil clears the
// override. Used for failure drills.
func (c *Checker) SetOverride(recKey string, up *bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.status[recKey]
	if !ok {
		st = &Status{RecKey: recKey, Up: true}
		c.status[recKey] = st
	}
	st.Forced = up
}

// Snapshot returns the status of all known targets.
func (c *Checker) Snapshot() []Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Status, 0, len(c.status))
	for _, st := range c.status {
		out = append(out, *st)
	}
	return out
}

// Run starts the check loop. targets is re-read from the given function on
// every tick so zone edits take effect.
func (c *Checker) Run(ctx context.Context, targets func() []Target) {
	ctx, c.cancel = context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(c.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.sweep(targets())
			}
		}
	}()
}

// Stop halts the check loop.
func (c *Checker) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *Checker) sweep(targets []Target) {
	seen := make(map[string]bool, len(targets))
	var wg sync.WaitGroup
	for _, t := range targets {
		seen[t.RecKey] = true
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			err := checkOnce(t.Check, c.timeout, c.client)
			c.mu.Lock()
			st, ok := c.status[t.RecKey]
			if !ok {
				st = &Status{RecKey: t.RecKey, Check: t.Check, Up: true}
				c.status[t.RecKey] = st
			}
			st.Check = t.Check
			st.LastCheck = time.Now()
			if err != nil {
				st.Failures++
				st.LastError = err.Error()
				if st.Failures >= c.FailThreshold {
					st.Up = false
				}
			} else {
				st.Failures = 0
				st.LastError = ""
				st.Up = true
			}
			c.mu.Unlock()
		}(t)
	}
	wg.Wait()
	// drop statuses for targets that no longer exist
	c.mu.Lock()
	for k := range c.status {
		if !seen[k] {
			delete(c.status, k)
		}
	}
	c.mu.Unlock()
}

func checkOnce(check string, timeout time.Duration, client *http.Client) error {
	switch {
	case strings.HasPrefix(check, "tcp://"):
		addr := strings.TrimPrefix(check, "tcp://")
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	case strings.HasPrefix(check, "http://"), strings.HasPrefix(check, "https://"):
		resp, err := client.Get(check)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("http status %d", resp.StatusCode)
		}
		return nil
	default:
		return fmt.Errorf("unknown check scheme in %q (want tcp:// or http://)", check)
	}
}
