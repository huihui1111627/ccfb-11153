package dnsmsg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
)

const (
	maxNameLen    = 255
	maxLabelLen   = 63
	maxPtrJumps   = 128
	headerLen     = 12
	maxMessageLen = 65535
)

// ErrFormat is returned for malformed messages.
var ErrFormat = errors.New("dns: malformed message")

// Fqdn ensures the name ends with a dot.
func Fqdn(s string) string {
	if s == "" || s == "." {
		return "."
	}
	if !strings.HasSuffix(s, ".") {
		return s + "."
	}
	return s
}

// packName appends a domain name to buf, using compression pointers into msg
// (the message built so far). comp maps lower-cased suffixes to offsets.
func packName(buf []byte, comp map[string]int, name string) ([]byte, error) {
	if name == "" || name == "." {
		return append(buf, 0), nil
	}
	name = strings.TrimSuffix(name, ".")
	labels := strings.Split(name, ".")
	for i := range labels {
		suffix := strings.ToLower(strings.Join(labels[i:], "."))
		if off, ok := comp[suffix]; ok && off < 0x3FFF {
			return append(buf, byte(0xC0|off>>8), byte(off)), nil
		}
		if len(buf) < 0x3FFF {
			comp[suffix] = len(buf)
		}
		l := labels[i]
		if len(l) == 0 || len(l) > maxLabelLen {
			return nil, fmt.Errorf("dns: bad label %q", l)
		}
		buf = append(buf, byte(len(l)))
		buf = append(buf, l...)
	}
	return append(buf, 0), nil
}

// unpackName reads a possibly-compressed domain name at off. It returns the
// name and the offset immediately after the name in the original stream
// (i.e. after the first pointer, if any). Pointer loops and out-of-range
// pointers are rejected.
func unpackName(msg []byte, off int) (string, int, error) {
	var labels []string
	pos := off
	end := -1 // offset after the name, set when we follow a pointer
	jumps := 0
	for {
		if pos >= len(msg) {
			return "", 0, ErrFormat
		}
		c := msg[pos]
		switch c & 0xC0 {
		case 0xC0: // pointer
			if pos+1 >= len(msg) {
				return "", 0, ErrFormat
			}
			ptr := int(c&0x3F)<<8 | int(msg[pos+1])
			if ptr >= pos { // pointers must go backwards: kills loops
				return "", 0, fmt.Errorf("dns: bad compression pointer to %d", ptr)
			}
			if end == -1 {
				end = pos + 2
			}
			jumps++
			if jumps > maxPtrJumps {
				return "", 0, fmt.Errorf("dns: too many compression pointers")
			}
			pos = ptr
		case 0x00: // plain label
			pos++
			if c == 0 {
				if end == -1 {
					end = pos
				}
				if len(labels) == 0 {
					return ".", end, nil
				}
				name := strings.Join(labels, ".") + "."
				if len(name) > maxNameLen {
					return "", 0, fmt.Errorf("dns: name too long")
				}
				return name, end, nil
			}
			l := int(c)
			if l > maxLabelLen || pos+l > len(msg) {
				return "", 0, ErrFormat
			}
			labels = append(labels, string(msg[pos:pos+l]))
			pos += l
		default:
			return "", 0, fmt.Errorf("dns: bad label type %02x", c)
		}
	}
}

// Pack serializes the message. If limit > 0, the output is kept within limit
// bytes: resource records that do not fit are dropped and the TC bit is set.
// The question section is always written in full.
func (m *Message) Pack(limit int) ([]byte, error) {
	if limit > 0 && limit < headerLen {
		return nil, fmt.Errorf("dns: limit %d too small", limit)
	}
	buf := make([]byte, headerLen, 512)
	comp := make(map[string]int)
	var err error
	for _, q := range m.Questions {
		if buf, err = packName(buf, comp, q.Name); err != nil {
			return nil, err
		}
		buf = append(buf, 0, 0, 0, 0)
		binary.BigEndian.PutUint16(buf[len(buf)-4:], q.Type)
		binary.BigEndian.PutUint16(buf[len(buf)-2:], q.Class)
	}
	tc := false
	counts := make([]int, 3)
	sections := [][]RR{m.Answers, m.Authorities, m.Additionals}
	truncated := false
	for si, sec := range sections {
		for _, rr := range sec {
			if truncated {
				continue
			}
			var nb []byte
			nb, err = packRR(buf, comp, rr)
			if err != nil {
				return nil, err
			}
			if limit > 0 && len(nb) > limit {
				truncated = true
				tc = true
				continue
			}
			buf = nb
			counts[si]++
		}
	}
	if limit > 0 && len(buf) > limit {
		return nil, fmt.Errorf("dns: message header+question exceed limit")
	}
	flags := uint16(m.Opcode&0xF) << 11
	if m.QR {
		flags |= 1 << 15
	}
	if m.AA {
		flags |= 1 << 10
	}
	if tc || m.TC {
		flags |= 1 << 9
	}
	if m.RD {
		flags |= 1 << 8
	}
	if m.RA {
		flags |= 1 << 7
	}
	flags |= uint16(m.RCode & 0xF)
	binary.BigEndian.PutUint16(buf[0:], m.ID)
	binary.BigEndian.PutUint16(buf[2:], flags)
	binary.BigEndian.PutUint16(buf[4:], uint16(len(m.Questions)))
	binary.BigEndian.PutUint16(buf[6:], uint16(counts[0]))
	binary.BigEndian.PutUint16(buf[8:], uint16(counts[1]))
	binary.BigEndian.PutUint16(buf[10:], uint16(counts[2]))
	return buf, nil
}

func packRR(buf []byte, comp map[string]int, rr RR) ([]byte, error) {
	var err error
	if buf, err = packName(buf, comp, rr.Name); err != nil {
		return nil, err
	}
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint16(hdr[0:], rr.Type)
	binary.BigEndian.PutUint16(hdr[2:], rr.Class)
	binary.BigEndian.PutUint32(hdr[4:], rr.TTL)
	buf = append(buf, hdr...)
	rdOff := len(buf)
	buf = append(buf, 0, 0) // rdlength placeholder
	if buf, err = packRData(buf, comp, rr); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint16(buf[rdOff:], uint16(len(buf)-rdOff-2))
	return buf, nil
}

func packRData(buf []byte, comp map[string]int, rr RR) ([]byte, error) {
	switch d := rr.Data.(type) {
	case *A:
		ip := d.IP.To4()
		if ip == nil {
			return nil, fmt.Errorf("dns: bad A address %v", d.IP)
		}
		return append(buf, ip...), nil
	case *AAAA:
		ip := d.IP.To16()
		if ip == nil {
			return nil, fmt.Errorf("dns: bad AAAA address %v", d.IP)
		}
		return append(buf, ip...), nil
	case *NS:
		return packName(buf, comp, d.Host)
	case *CNAME:
		return packName(buf, comp, d.Target)
	case *PTR:
		return packName(buf, comp, d.Target)
	case *MX:
		buf = append(buf, byte(d.Preference>>8), byte(d.Preference))
		return packName(buf, comp, d.Host)
	case *TXT:
		for _, t := range d.Texts {
			if len(t) > 255 {
				return nil, fmt.Errorf("dns: TXT string too long")
			}
			buf = append(buf, byte(len(t)))
			buf = append(buf, t...)
		}
		return buf, nil
	case *SOA:
		var err error
		if buf, err = packName(buf, comp, d.NS); err != nil {
			return nil, err
		}
		if buf, err = packName(buf, comp, d.Mbox); err != nil {
			return nil, err
		}
		var tmp [20]byte
		binary.BigEndian.PutUint32(tmp[0:], d.Serial)
		binary.BigEndian.PutUint32(tmp[4:], d.Refresh)
		binary.BigEndian.PutUint32(tmp[8:], d.Retry)
		binary.BigEndian.PutUint32(tmp[12:], d.Expire)
		binary.BigEndian.PutUint32(tmp[16:], d.MinTTL)
		return append(buf, tmp[:]...), nil
	case *SRV:
		buf = append(buf,
			byte(d.Priority>>8), byte(d.Priority),
			byte(d.Weight>>8), byte(d.Weight),
			byte(d.Port>>8), byte(d.Port))
		return packName(buf, comp, d.Target)
	case *OPT:
		for _, opt := range d.Options {
			buf = append(buf, byte(opt.Code>>8), byte(opt.Code),
				byte(len(opt.Data)>>8), byte(len(opt.Data)))
			buf = append(buf, opt.Data...)
		}
		return buf, nil
	case *Unknown:
		return append(buf, d.Data...), nil
	case nil:
		return buf, nil
	default:
		return nil, fmt.Errorf("dns: unsupported rdata %T", d)
	}
}

// Unpack parses a DNS message. Trailing garbage is rejected.
func Unpack(msg []byte) (*Message, error) {
	if len(msg) < headerLen {
		return nil, ErrFormat
	}
	m := &Message{}
	m.ID = binary.BigEndian.Uint16(msg[0:])
	flags := binary.BigEndian.Uint16(msg[2:])
	m.QR = flags&(1<<15) != 0
	m.Opcode = uint8(flags>>11) & 0xF
	m.AA = flags&(1<<10) != 0
	m.TC = flags&(1<<9) != 0
	m.RD = flags&(1<<8) != 0
	m.RA = flags&(1<<7) != 0
	m.RCode = uint8(flags & 0xF)
	qd := int(binary.BigEndian.Uint16(msg[4:]))
	an := int(binary.BigEndian.Uint16(msg[6:]))
	ns := int(binary.BigEndian.Uint16(msg[8:]))
	ar := int(binary.BigEndian.Uint16(msg[10:]))

	off := headerLen
	var err error
	for i := 0; i < qd; i++ {
		var q Question
		if q.Name, off, err = unpackName(msg, off); err != nil {
			return nil, err
		}
		if off+4 > len(msg) {
			return nil, ErrFormat
		}
		q.Type = binary.BigEndian.Uint16(msg[off:])
		q.Class = binary.BigEndian.Uint16(msg[off+2:])
		off += 4
		m.Questions = append(m.Questions, q)
	}
	readRRs := func(n int) ([]RR, error) {
		var out []RR
		for i := 0; i < n; i++ {
			var rr RR
			if rr, off, err = unpackRR(msg, off); err != nil {
				return nil, err
			}
			out = append(out, rr)
		}
		return out, nil
	}
	if m.Answers, err = readRRs(an); err != nil {
		return nil, err
	}
	if m.Authorities, err = readRRs(ns); err != nil {
		return nil, err
	}
	if m.Additionals, err = readRRs(ar); err != nil {
		return nil, err
	}
	if off != len(msg) {
		return nil, fmt.Errorf("dns: %d trailing bytes", len(msg)-off)
	}
	return m, nil
}

func unpackRR(msg []byte, off int) (RR, int, error) {
	var rr RR
	var err error
	if rr.Name, off, err = unpackName(msg, off); err != nil {
		return rr, 0, err
	}
	if off+10 > len(msg) {
		return rr, 0, ErrFormat
	}
	rr.Type = binary.BigEndian.Uint16(msg[off:])
	rr.Class = binary.BigEndian.Uint16(msg[off+2:])
	rr.TTL = binary.BigEndian.Uint32(msg[off+4:])
	rdlen := int(binary.BigEndian.Uint16(msg[off+8:]))
	off += 10
	if off+rdlen > len(msg) {
		return rr, 0, ErrFormat
	}
	end := off + rdlen
	if rr.Data, err = unpackRData(msg, off, end, rr.Type); err != nil {
		return rr, 0, err
	}
	return rr, end, nil
}

func unpackRData(msg []byte, off, end int, rtype uint16) (RData, error) {
	rd := msg[off:end]
	switch rtype {
	case TypeA:
		if len(rd) != 4 {
			return nil, ErrFormat
		}
		ip := make(net.IP, net.IPv4len)
		copy(ip, rd)
		return &A{IP: ip}, nil
	case TypeAAAA:
		if len(rd) != 16 {
			return nil, ErrFormat
		}
		ip := make(net.IP, net.IPv6len)
		copy(ip, rd)
		return &AAAA{IP: ip}, nil
	case TypeNS:
		n, _, err := unpackName(msg, off)
		if err != nil {
			return nil, err
		}
		return &NS{Host: n}, nil
	case TypeCNAME:
		n, _, err := unpackName(msg, off)
		if err != nil {
			return nil, err
		}
		return &CNAME{Target: n}, nil
	case TypePTR:
		n, _, err := unpackName(msg, off)
		if err != nil {
			return nil, err
		}
		return &PTR{Target: n}, nil
	case TypeMX:
		if len(rd) < 3 {
			return nil, ErrFormat
		}
		n, _, err := unpackName(msg, off+2)
		if err != nil {
			return nil, err
		}
		return &MX{Preference: binary.BigEndian.Uint16(rd), Host: n}, nil
	case TypeTXT:
		var texts []string
		for i := 0; i < len(rd); {
			l := int(rd[i])
			i++
			if i+l > len(rd) {
				return nil, ErrFormat
			}
			texts = append(texts, string(rd[i:i+l]))
			i += l
		}
		return &TXT{Texts: texts}, nil
	case TypeSOA:
		ns, o1, err := unpackName(msg, off)
		if err != nil {
			return nil, err
		}
		mb, o2, err := unpackName(msg, o1)
		if err != nil {
			return nil, err
		}
		if o2+20 > end {
			return nil, ErrFormat
		}
		return &SOA{
			NS:      ns,
			Mbox:    mb,
			Serial:  binary.BigEndian.Uint32(msg[o2:]),
			Refresh: binary.BigEndian.Uint32(msg[o2+4:]),
			Retry:   binary.BigEndian.Uint32(msg[o2+8:]),
			Expire:  binary.BigEndian.Uint32(msg[o2+12:]),
			MinTTL:  binary.BigEndian.Uint32(msg[o2+16:]),
		}, nil
	case TypeSRV:
		if len(rd) < 7 {
			return nil, ErrFormat
		}
		n, _, err := unpackName(msg, off+6)
		if err != nil {
			return nil, err
		}
		return &SRV{
			Priority: binary.BigEndian.Uint16(rd[0:]),
			Weight:   binary.BigEndian.Uint16(rd[2:]),
			Port:     binary.BigEndian.Uint16(rd[4:]),
			Target:   n,
		}, nil
	case TypeOPT:
		opt := &OPT{}
		for i := 0; i+4 <= len(rd); {
			code := binary.BigEndian.Uint16(rd[i:])
			l := int(binary.BigEndian.Uint16(rd[i+2:]))
			i += 4
			if i+l > len(rd) {
				return nil, ErrFormat
			}
			data := make([]byte, l)
			copy(data, rd[i:i+l])
			opt.Options = append(opt.Options, EDNSOption{Code: code, Data: data})
			i += l
		}
		return opt, nil
	default:
		data := make([]byte, len(rd))
		copy(data, rd)
		return &Unknown{Data: data}, nil
	}
}
