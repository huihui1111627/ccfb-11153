// Package health actively checks A/AAAA endpoints and tracks which are up.
package health

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Target is one endpoint to probe.
type Target struct {
	IP     string `json:"ip"`
	Type   string `json:"type"` // "tcp" or "http"
	Port   int    `json:"port"`
	Path   string `json:"path"`
	Zone   string `json:"zone"`
	Record string `json:"record"`
	DC     string `json:"dc"`
}

// Status is the health view of one target.
type Status struct {
	Target
	Healthy   bool      `json:"healthy"`
	LastCheck time.Time `json:"last_check"`
	LastError string    `json:"last_error,omitempty"`
}

// Checker probes targets periodically. Targets whose zone has health
// checking disabled are simply absent and treated as healthy.
type Checker struct {
	mu       sync.RWMutex
	targets  map[string]Target // key: ip
	status   map[string]*Status
	interval time.Duration
	timeout  time.Duration
	stop     chan struct{}
	onChange func(ip string, healthy bool)
}

// New creates a checker. onChange fires when a target flips state.
func New(interval, timeout time.Duration, onChange func(ip string, healthy bool)) *Checker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Checker{
		targets:  map[string]Target{},
		status:   map[string]*Status{},
		interval: interval,
		timeout:  timeout,
		stop:     make(chan struct{}),
		onChange: onChange,
	}
}

// SetTargets replaces the target set (called when zone data changes).
func (c *Checker) SetTargets(ts []Target) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.targets = map[string]Target{}
	for _, t := range ts {
		c.targets[t.IP] = t
		if _, ok := c.status[t.IP]; !ok {
			c.status[t.IP] = &Status{Target: t, Healthy: true} // assume up until proven down
		}
	}
	for ip := range c.status {
		if _, ok := c.targets[ip]; !ok {
			delete(c.status, ip)
		}
	}
}

// Healthy reports whether ip is currently considered up. Unknown IPs
// (no check configured) are healthy.
func (c *Checker) Healthy(ip string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st, ok := c.status[ip]
	if !ok {
		return true
	}
	return st.Healthy
}

// Snapshot returns the status of all targets.
func (c *Checker) Snapshot() []Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Status, 0, len(c.status))
	for _, s := range c.status {
		out = append(out, *s)
	}
	return out
}

// Run starts the probe loop until Stop is called.
func (c *Checker) Run() {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			c.probeAll()
		}
	}
}

// Stop ends the probe loop.
func (c *Checker) Stop() { close(c.stop) }

// ProbeNow forces an immediate probe of all targets (used by tests/UI).
func (c *Checker) ProbeNow() { c.probeAll() }

func (c *Checker) probeAll() {
	c.mu.RLock()
	ts := make([]Target, 0, len(c.targets))
	for _, t := range c.targets {
		ts = append(ts, t)
	}
	c.mu.RUnlock()

	var wg sync.WaitGroup
	for _, tgt := range ts {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			err := c.probe(t)
			healthy := err == nil
			errStr := ""
			if err != nil {
				errStr = err.Error()
			}
			c.mu.Lock()
			st := c.status[t.IP]
			flipped := st.Healthy != healthy
			st.Healthy = healthy
			st.LastCheck = time.Now()
			st.LastError = errStr
			c.mu.Unlock()
			if flipped && c.onChange != nil {
				c.onChange(t.IP, healthy)
			}
		}(tgt)
	}
	wg.Wait()
}

func (c *Checker) probe(t Target) error {
	addr := net.JoinHostPort(t.IP, fmt.Sprint(t.Port))
	switch t.Type {
	case "http":
		path := t.Path
		if path == "" {
			path = "/"
		}
		url := fmt.Sprintf("http://%s%s", addr, path)
		cli := &http.Client{Timeout: c.timeout}
		resp, err := cli.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("http status %d", resp.StatusCode)
		}
		return nil
	default: // tcp
		conn, err := net.DialTimeout("tcp", addr, c.timeout)
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	}
}
