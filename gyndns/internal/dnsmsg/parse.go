package dnsmsg

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ParseRData builds RData from zone-file presentation tokens. Domain names in
// the tokens must already be fully qualified.
func ParseRData(rtype uint16, f []string) (RData, error) {
	need := func(n int) error {
		if len(f) < n {
			return fmt.Errorf("%s needs at least %d fields, got %d", TypeString(rtype), n, len(f))
		}
		return nil
	}
	switch rtype {
	case TypeA:
		if err := need(1); err != nil {
			return nil, err
		}
		ip := net.ParseIP(f[0]).To4()
		if ip == nil {
			return nil, fmt.Errorf("bad A address %q", f[0])
		}
		return &A{IP: ip}, nil
	case TypeAAAA:
		if err := need(1); err != nil {
			return nil, err
		}
		ip := net.ParseIP(f[0])
		if ip == nil || ip.To4() != nil {
			return nil, fmt.Errorf("bad AAAA address %q", f[0])
		}
		return &AAAA{IP: ip.To16()}, nil
	case TypeNS:
		if err := need(1); err != nil {
			return nil, err
		}
		return &NS{Host: Fqdn(f[0])}, nil
	case TypeCNAME:
		if err := need(1); err != nil {
			return nil, err
		}
		return &CNAME{Target: Fqdn(f[0])}, nil
	case TypePTR:
		if err := need(1); err != nil {
			return nil, err
		}
		return &PTR{Target: Fqdn(f[0])}, nil
	case TypeMX:
		if err := need(2); err != nil {
			return nil, err
		}
		p, err := strconv.ParseUint(f[0], 10, 16)
		if err != nil {
			return nil, fmt.Errorf("bad MX preference: %v", err)
		}
		return &MX{Preference: uint16(p), Host: Fqdn(f[1])}, nil
	case TypeTXT:
		if err := need(1); err != nil {
			return nil, err
		}
		return &TXT{Texts: f}, nil
	case TypeSOA:
		if err := need(7); err != nil {
			return nil, err
		}
		nums := make([]uint32, 5)
		for i := 0; i < 5; i++ {
			v, err := strconv.ParseUint(f[2+i], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("bad SOA number %q: %v", f[2+i], err)
			}
			nums[i] = uint32(v)
		}
		return &SOA{
			NS: Fqdn(f[0]), Mbox: Fqdn(f[1]),
			Serial: nums[0], Refresh: nums[1], Retry: nums[2],
			Expire: nums[3], MinTTL: nums[4],
		}, nil
	case TypeSRV:
		if err := need(4); err != nil {
			return nil, err
		}
		var nums [3]uint16
		for i := 0; i < 3; i++ {
			v, err := strconv.ParseUint(f[i], 10, 16)
			if err != nil {
				return nil, fmt.Errorf("bad SRV number %q: %v", f[i], err)
			}
			nums[i] = uint16(v)
		}
		return &SRV{Priority: nums[0], Weight: nums[1], Port: nums[2], Target: Fqdn(f[3])}, nil
	default:
		return nil, fmt.Errorf("cannot parse rdata for %s", TypeString(rtype))
	}
}

// ParseRDataString parses rdata from a single presentation-format string.
// TXT fields may be double-quoted.
func ParseRDataString(rtype uint16, s string) (RData, error) {
	return ParseRData(rtype, SplitFields(s))
}

// SplitFields splits a presentation-format string, honoring double quotes.
func SplitFields(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	has := false
	flush := func() {
		if has || cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
			has = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			has = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			has = true
		case (c == ' ' || c == '\t') && !inQuote:
			flush()
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	flush()
	return out
}
