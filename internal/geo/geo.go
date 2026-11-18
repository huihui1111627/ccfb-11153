// Package geo maps client subnets to the nearest datacenter and derives
// rate-limit/statistics subnet keys.
package geo

import (
	"net"
	"sort"
	"strings"
)

// SubnetRule binds a CIDR to a datacenter and region label.
type SubnetRule struct {
	CIDR   string `json:"cidr"`
	DC     string `json:"dc"`
	Region string `json:"region"`

	parsed *net.IPNet
}

// Matcher does longest-prefix-match lookup of client IPs.
type Matcher struct {
	rules []*SubnetRule
}

// NewMatcher compiles the rules; invalid CIDRs are skipped.
func NewMatcher(rules []SubnetRule) *Matcher {
	m := &Matcher{}
	for i := range rules {
		r := rules[i]
		_, n, err := net.ParseCIDR(r.CIDR)
		if err != nil {
			continue
		}
		r.parsed = n
		m.rules = append(m.rules, &r)
	}
	// Longest prefix first.
	sort.Slice(m.rules, func(i, j int) bool {
		si, _ := m.rules[i].parsed.Mask.Size()
		sj, _ := m.rules[j].parsed.Mask.Size()
		return si > sj
	})
	return m
}

// Lookup returns the datacenter and region for ip.
func (m *Matcher) Lookup(ip net.IP) (dc, region string, ok bool) {
	if m == nil {
		return "", "", false
	}
	for _, r := range m.rules {
		if r.parsed.Contains(ip) {
			return r.DC, r.Region, true
		}
	}
	return "", "", false
}

// Rules returns the compiled rules (for the web UI).
func (m *Matcher) Rules() []SubnetRule {
	out := make([]SubnetRule, 0, len(m.rules))
	for _, r := range m.rules {
		out = append(out, *r)
	}
	return out
}

// SubnetKey masks ip to the given prefix lengths and returns the network
// string, used for per-subnet rate limiting and statistics.
func SubnetKey(ip net.IP, v4len, v6len int) string {
	if ip4 := ip.To4(); ip4 != nil {
		if v4len <= 0 {
			v4len = 24
		}
		n := &net.IPNet{IP: ip4.Mask(net.CIDRMask(v4len, 32)), Mask: net.CIDRMask(v4len, 32)}
		return n.String()
	}
	if v6len <= 0 {
		v6len = 56
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return "unknown"
	}
	n := &net.IPNet{IP: ip16.Mask(net.CIDRMask(v6len, 128)), Mask: net.CIDRMask(v6len, 128)}
	return n.String()
}

// NormalizeRuleList parses "cidr=dc:region" shorthand lines (used in config).
func NormalizeRuleList(rules []SubnetRule) []SubnetRule {
	out := make([]SubnetRule, 0, len(rules))
	for _, r := range rules {
		r.CIDR = strings.TrimSpace(r.CIDR)
		if r.CIDR == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}
