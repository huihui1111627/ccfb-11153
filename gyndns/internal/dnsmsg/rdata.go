package dnsmsg

import (
	"fmt"
	"net"
	"strings"
)

// RData is the type-specific payload of a resource record.
type RData interface {
	rdata()
	String() string
}

// A is an IPv4 address record.
type A struct{ IP net.IP }

// AAAA is an IPv6 address record.
type AAAA struct{ IP net.IP }

// NS is a name server record.
type NS struct{ Host string }

// CNAME is a canonical name record.
type CNAME struct{ Target string }

// PTR is a pointer record.
type PTR struct{ Target string }

// MX is a mail exchange record.
type MX struct {
	Preference uint16
	Host       string
}

// TXT is a text record; each entry is one character-string.
type TXT struct{ Texts []string }

// SOA marks the start of a zone of authority.
type SOA struct {
	NS      string
	Mbox    string
	Serial  uint32
	Refresh uint32
	Retry   uint32
	Expire  uint32
	MinTTL  uint32
}

// SRV is a service locator record.
type SRV struct {
	Priority uint16
	Weight   uint16
	Port     uint16
	Target   string
}

// OPT is the EDNS0 pseudo-record payload.
type OPT struct {
	Options []EDNSOption
}

// EDNSOption is a single EDNS option.
type EDNSOption struct {
	Code uint16
	Data []byte
}

// Unknown carries the raw payload of record types we do not interpret (RFC 3597).
type Unknown struct{ Data []byte }

func (*A) rdata()       {}
func (*AAAA) rdata()    {}
func (*NS) rdata()      {}
func (*CNAME) rdata()   {}
func (*PTR) rdata()     {}
func (*MX) rdata()      {}
func (*TXT) rdata()     {}
func (*SOA) rdata()     {}
func (*SRV) rdata()     {}
func (*OPT) rdata()     {}
func (*Unknown) rdata() {}

func (d *A) String() string     { return d.IP.String() }
func (d *AAAA) String() string  { return d.IP.String() }
func (d *NS) String() string    { return d.Host }
func (d *CNAME) String() string { return d.Target }
func (d *PTR) String() string   { return d.Target }
func (d *MX) String() string    { return fmt.Sprintf("%d %s", d.Preference, d.Host) }
func (d *TXT) String() string {
	var b strings.Builder
	for i, t := range d.Texts {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%q", t)
	}
	return b.String()
}
func (d *SOA) String() string {
	return fmt.Sprintf("%s %s %d %d %d %d %d", d.NS, d.Mbox, d.Serial, d.Refresh, d.Retry, d.Expire, d.MinTTL)
}
func (d *SRV) String() string {
	return fmt.Sprintf("%d %d %d %s", d.Priority, d.Weight, d.Port, d.Target)
}
func (d *OPT) String() string     { return fmt.Sprintf("%d options", len(d.Options)) }
func (d *Unknown) String() string { return fmt.Sprintf("\\# %d %x", len(d.Data), d.Data) }

// RR is a resource record.
type RR struct {
	Name  string
	Type  uint16
	Class uint16
	TTL   uint32
	Data  RData
}

// String renders the record in zone-file presentation format.
func (rr RR) String() string {
	return fmt.Sprintf("%s %d IN %s %s", rr.Name, rr.TTL, TypeString(rr.Type), rr.Data.String())
}

// ECSOption extracts the EDNS Client Subnet option, if present.
func (o *OPT) ECSOption() (prefix *net.IPNet, ok bool) {
	for _, opt := range o.Options {
		if opt.Code != EDNSOptionECS || len(opt.Data) < 4 {
			continue
		}
		family := uint16(opt.Data[0])<<8 | uint16(opt.Data[1])
		bits := int(opt.Data[2])
		addrBytes := opt.Data[4:]
		var ip net.IP
		switch family {
		case 1:
			ip = make(net.IP, net.IPv4len)
			copy(ip, addrBytes)
		case 2:
			ip = make(net.IP, net.IPv6len)
			copy(ip, addrBytes)
		default:
			continue
		}
		return &net.IPNet{IP: ip.Mask(net.CIDRMask(bits, len(ip)*8)), Mask: net.CIDRMask(bits, len(ip)*8)}, true
	}
	return nil, false
}

// ClientSubnet returns the client subnet signalled via EDNS ECS, if any.
func (m *Message) ClientSubnet() *net.IPNet {
	for _, rr := range m.Additionals {
		if rr.Type != TypeOPT {
			continue
		}
		if opt, ok := rr.Data.(*OPT); ok {
			if p, ok := opt.ECSOption(); ok {
				return p
			}
		}
	}
	return nil
}
