// Package dnsmsg implements DNS message (RFC 1035) encoding and decoding
// from scratch, including name compression and minimal EDNS(0) (RFC 6891).
package dnsmsg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Record types.
const (
	TypeA     uint16 = 1
	TypeNS    uint16 = 2
	TypeCNAME uint16 = 5
	TypeSOA   uint16 = 6
	TypePTR   uint16 = 12
	TypeMX    uint16 = 15
	TypeTXT   uint16 = 16
	TypeAAAA  uint16 = 28
	TypeSRV   uint16 = 33
	TypeOPT   uint16 = 41
	TypeAXFR  uint16 = 252
	TypeANY   uint16 = 255
)

// Classes.
const ClassIN uint16 = 1

// Response codes.
const (
	RcodeNoError  uint8 = 0
	RcodeFormErr  uint8 = 1
	RcodeServFail uint8 = 2
	RcodeNXDomain uint8 = 3
	RcodeNotImp   uint8 = 4
	RcodeRefused  uint8 = 5
)

var (
	// TypeNames maps a type code to its mnemonic.
	TypeNames = map[uint16]string{
		TypeA: "A", TypeNS: "NS", TypeCNAME: "CNAME", TypeSOA: "SOA",
		TypePTR: "PTR", TypeMX: "MX", TypeTXT: "TXT", TypeAAAA: "AAAA",
		TypeSRV: "SRV", TypeOPT: "OPT", TypeAXFR: "AXFR", TypeANY: "ANY",
	}
	// NameToType maps a mnemonic to its type code.
	NameToType = map[string]uint16{
		"A": TypeA, "NS": TypeNS, "CNAME": TypeCNAME, "SOA": TypeSOA,
		"PTR": TypePTR, "MX": TypeMX, "TXT": TypeTXT, "AAAA": TypeAAAA,
		"SRV": TypeSRV, "ANY": TypeANY,
	}
)

// TypeString returns the mnemonic for t, or TYPE<n> if unknown.
func TypeString(t uint16) string {
	if s, ok := TypeNames[t]; ok {
		return s
	}
	return fmt.Sprintf("TYPE%d", t)
}

// Header is the 12-byte DNS message header.
type Header struct {
	ID     uint16
	QR     bool
	Opcode uint8
	AA     bool
	TC     bool
	RD     bool
	RA     bool
	Rcode  uint8
}

// Question is a DNS question entry.
type Question struct {
	Name   string // FQDN with trailing dot
	Qtype  uint16
	Qclass uint16
}

// RR is a resource record with decoded rdata for the supported types.
// Raw holds undecoded rdata for unknown types.
type RR struct {
	Name  string // owner, FQDN with trailing dot
	Type  uint16
	Class uint16
	TTL   uint32

	IP     net.IP   // A, AAAA
	Target string   // CNAME, NS, PTR, MX exchange, SRV target, SOA mname
	Text   []string // TXT

	Preference uint16 // MX
	Priority   uint16 // SRV
	Weight     uint16 // SRV
	Port       uint16 // SRV

	Rname   string // SOA
	Serial  uint32 // SOA
	Refresh uint32 // SOA
	Retry   uint32 // SOA
	Expire  uint32 // SOA
	Minimum uint32 // SOA

	Raw []byte // unknown types
}

// Message is a full DNS message.
type Message struct {
	Header
	Questions   []Question
	Answers     []RR
	Authorities []RR
	Additionals []RR

	// EDNSSize is the requestor's UDP payload size from the OPT record;
	// zero means the peer did not send EDNS.
	EDNSSize int
}

// Normalize lowercases name and ensures a trailing dot.
func Normalize(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == "." {
		return "."
	}
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	return name
}

var (
	errShort   = errors.New("dnsmsg: message too short")
	errBadName = errors.New("dnsmsg: invalid name")
)

// Unpack parses a wire-format DNS message.
func Unpack(msg []byte) (*Message, error) {
	if len(msg) < 12 {
		return nil, errShort
	}
	be := binary.BigEndian
	m := &Message{}
	m.ID = be.Uint16(msg[0:2])
	flags := be.Uint16(msg[2:4])
	m.QR = flags&0x8000 != 0
	m.Opcode = uint8(flags>>11) & 0xF
	m.AA = flags&0x0400 != 0
	m.TC = flags&0x0200 != 0
	m.RD = flags&0x0100 != 0
	m.RA = flags&0x0080 != 0
	m.Rcode = uint8(flags & 0xF)
	qd := int(be.Uint16(msg[4:6]))
	an := int(be.Uint16(msg[6:8]))
	ns := int(be.Uint16(msg[8:10]))
	ar := int(be.Uint16(msg[10:12]))

	off := 12
	var err error
	for i := 0; i < qd; i++ {
		var q Question
		q.Name, off, err = unpackName(msg, off)
		if err != nil {
			return nil, err
		}
		if off+4 > len(msg) {
			return nil, errShort
		}
		q.Qtype = be.Uint16(msg[off : off+2])
		q.Qclass = be.Uint16(msg[off+2 : off+4])
		off += 4
		m.Questions = append(m.Questions, q)
	}
	if m.Questions == nil {
		m.Questions = []Question{}
	}
	readRRs := func(n int, dst *[]RR) error {
		for i := 0; i < n; i++ {
			rr, noff, e := unpackRR(msg, off)
			if e != nil {
				return e
			}
			off = noff
			if rr.Type == TypeOPT {
				m.EDNSSize = int(rr.Class)
				continue
			}
			*dst = append(*dst, rr)
		}
		return nil
	}
	if err = readRRs(an, &m.Answers); err != nil {
		return nil, err
	}
	if err = readRRs(ns, &m.Authorities); err != nil {
		return nil, err
	}
	if err = readRRs(ar, &m.Additionals); err != nil {
		return nil, err
	}
	return m, nil
}

// unpackName decodes a possibly-compressed domain name at off.
// Returns the name (FQDN, trailing dot) and the offset just past the name
// in the original (uncompressed) position.
func unpackName(msg []byte, off int) (string, int, error) {
	var sb strings.Builder
	cur := off
	end := -1
	jumps := 0
	for {
		if cur >= len(msg) {
			return "", 0, errShort
		}
		l := int(msg[cur])
		switch {
		case l == 0:
			if end < 0 {
				end = cur + 1
			}
			if sb.Len() == 0 {
				return ".", end, nil
			}
			return sb.String(), end, nil
		case l&0xC0 == 0xC0:
			if cur+1 >= len(msg) {
				return "", 0, errShort
			}
			ptr := (l&0x3F)<<8 | int(msg[cur+1])
			if ptr >= cur {
				return "", 0, fmt.Errorf("dnsmsg: compression pointer not backward (ptr=%d cur=%d)", ptr, cur)
			}
			if end < 0 {
				end = cur + 2
			}
			cur = ptr
			jumps++
			if jumps > 32 {
				return "", 0, fmt.Errorf("dnsmsg: too many compression jumps")
			}
		case l&0xC0 != 0:
			return "", 0, errBadName
		default:
			if cur+1+l > len(msg) {
				return "", 0, errShort
			}
			sb.Write(msg[cur+1 : cur+1+l])
			sb.WriteByte('.')
			cur += 1 + l
		}
	}
}

func unpackRR(msg []byte, off int) (RR, int, error) {
	var rr RR
	be := binary.BigEndian
	name, noff, err := unpackName(msg, off)
	if err != nil {
		return rr, 0, err
	}
	off = noff
	if off+10 > len(msg) {
		return rr, 0, errShort
	}
	rr.Name = name
	rr.Type = be.Uint16(msg[off : off+2])
	rr.Class = be.Uint16(msg[off+2 : off+4])
	rr.TTL = be.Uint32(msg[off+4 : off+8])
	rdlen := int(be.Uint16(msg[off+8 : off+10]))
	off += 10
	if off+rdlen > len(msg) {
		return rr, 0, errShort
	}
	rdata := msg[off : off+rdlen]
	end := off + rdlen

	switch rr.Type {
	case TypeA:
		if rdlen == 4 {
			rr.IP = net.IP(append([]byte(nil), rdata...))
		}
	case TypeAAAA:
		if rdlen == 16 {
			rr.IP = net.IP(append([]byte(nil), rdata...))
		}
	case TypeCNAME, TypeNS, TypePTR:
		t, _, e := unpackName(msg, off)
		if e != nil {
			return rr, 0, e
		}
		rr.Target = t
	case TypeMX:
		if rdlen < 3 {
			return rr, 0, errShort
		}
		rr.Preference = be.Uint16(msg[off : off+2])
		t, _, e := unpackName(msg, off+2)
		if e != nil {
			return rr, 0, e
		}
		rr.Target = t
	case TypeTXT:
		i := 0
		for i < len(rdata) {
			l := int(rdata[i])
			if i+1+l > len(rdata) {
				return rr, 0, errShort
			}
			rr.Text = append(rr.Text, string(rdata[i+1:i+1+l]))
			i += 1 + l
		}
	case TypeSRV:
		if rdlen < 7 {
			return rr, 0, errShort
		}
		rr.Priority = be.Uint16(msg[off : off+2])
		rr.Weight = be.Uint16(msg[off+2 : off+4])
		rr.Port = be.Uint16(msg[off+4 : off+6])
		t, _, e := unpackName(msg, off+6)
		if e != nil {
			return rr, 0, e
		}
		rr.Target = t
	case TypeSOA:
		mname, n1, e := unpackName(msg, off)
		if e != nil {
			return rr, 0, e
		}
		rname, n2, e := unpackName(msg, n1)
		if e != nil {
			return rr, 0, e
		}
		if n2+20 > end {
			return rr, 0, errShort
		}
		rr.Target = mname
		rr.Rname = rname
		rr.Serial = be.Uint32(msg[n2 : n2+4])
		rr.Refresh = be.Uint32(msg[n2+4 : n2+8])
		rr.Retry = be.Uint32(msg[n2+8 : n2+12])
		rr.Expire = be.Uint32(msg[n2+12 : n2+16])
		rr.Minimum = be.Uint32(msg[n2+16 : n2+20])
	default:
		rr.Raw = append([]byte(nil), rdata...)
	}
	return rr, end, nil
}

// Pack serializes the message. If compress is true, owner names are
// compressed with pointers (RFC 1035 §4.1.4).
func (m *Message) Pack(compress bool) ([]byte, error) {
	buf := make([]byte, 12, 512)
	comp := map[string]int{}
	var err error
	for _, q := range m.Questions {
		if buf, err = packName(buf, q.Name, comp, compress); err != nil {
			return nil, err
		}
		buf = binary.BigEndian.AppendUint16(buf, q.Qtype)
		buf = binary.BigEndian.AppendUint16(buf, q.Qclass)
	}
	for _, rr := range m.Answers {
		if buf, err = packRR(buf, rr, comp, compress); err != nil {
			return nil, err
		}
	}
	for _, rr := range m.Authorities {
		if buf, err = packRR(buf, rr, comp, compress); err != nil {
			return nil, err
		}
	}
	for _, rr := range m.Additionals {
		if buf, err = packRR(buf, rr, comp, compress); err != nil {
			return nil, err
		}
	}
	// Echo an OPT record if the request used EDNS.
	if m.EDNSSize > 0 {
		buf = append(buf, 0) // root name
		buf = binary.BigEndian.AppendUint16(buf, TypeOPT)
		size := m.EDNSSize
		if size > 4096 {
			size = 4096
		}
		buf = binary.BigEndian.AppendUint16(buf, uint16(size))
		buf = binary.BigEndian.AppendUint32(buf, 0) // extended rcode/flags
		buf = binary.BigEndian.AppendUint16(buf, 0) // rdlen
	}
	be := binary.BigEndian
	flags := uint16(m.Rcode) | uint16(m.Opcode)<<11
	if m.QR {
		flags |= 0x8000
	}
	if m.AA {
		flags |= 0x0400
	}
	if m.TC {
		flags |= 0x0200
	}
	if m.RD {
		flags |= 0x0100
	}
	if m.RA {
		flags |= 0x0080
	}
	ar := len(m.Additionals)
	if m.EDNSSize > 0 {
		ar++
	}
	be.PutUint16(buf[0:2], m.ID)
	be.PutUint16(buf[2:4], flags)
	be.PutUint16(buf[4:6], uint16(len(m.Questions)))
	be.PutUint16(buf[6:8], uint16(len(m.Answers)))
	be.PutUint16(buf[8:10], uint16(len(m.Authorities)))
	be.PutUint16(buf[10:12], uint16(ar))
	return buf, nil
}

func packName(buf []byte, name string, comp map[string]int, compress bool) ([]byte, error) {
	name = Normalize(name)
	if name == "." {
		return append(buf, 0), nil
	}
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i, lab := range labels {
		if len(lab) == 0 || len(lab) > 63 {
			return nil, fmt.Errorf("dnsmsg: bad label length in %q", name)
		}
		suffix := strings.ToLower(strings.Join(labels[i:], "."))
		if compress {
			if off, ok := comp[suffix]; ok && off < 0x4000 {
				return append(buf, byte(0xC0|(off>>8)), byte(off)), nil
			}
			if len(buf) < 0x4000 {
				comp[suffix] = len(buf)
			}
		}
		buf = append(buf, byte(len(lab)))
		buf = append(buf, lab...)
	}
	return append(buf, 0), nil
}

func packRR(buf []byte, rr RR, comp map[string]int, compress bool) ([]byte, error) {
	var err error
	if buf, err = packName(buf, rr.Name, comp, compress); err != nil {
		return nil, err
	}
	buf = binary.BigEndian.AppendUint16(buf, rr.Type)
	buf = binary.BigEndian.AppendUint16(buf, rr.Class)
	buf = binary.BigEndian.AppendUint32(buf, rr.TTL)
	lenPos := len(buf)
	buf = append(buf, 0, 0) // rdlength placeholder
	start := len(buf)

	switch rr.Type {
	case TypeA:
		ip4 := rr.IP.To4()
		if ip4 == nil {
			return nil, fmt.Errorf("dnsmsg: bad A rdata for %s", rr.Name)
		}
		buf = append(buf, ip4...)
	case TypeAAAA:
		ip16 := rr.IP.To16()
		if ip16 == nil || rr.IP.To4() != nil {
			return nil, fmt.Errorf("dnsmsg: bad AAAA rdata for %s", rr.Name)
		}
		buf = append(buf, ip16...)
	case TypeCNAME, TypeNS, TypePTR:
		if buf, err = packName(buf, rr.Target, comp, false); err != nil {
			return nil, err
		}
	case TypeMX:
		buf = binary.BigEndian.AppendUint16(buf, rr.Preference)
		if buf, err = packName(buf, rr.Target, comp, false); err != nil {
			return nil, err
		}
	case TypeTXT:
		for _, s := range rr.Text {
			for len(s) > 255 {
				buf = append(buf, 255)
				buf = append(buf, s[:255]...)
				s = s[255:]
			}
			buf = append(buf, byte(len(s)))
			buf = append(buf, s...)
		}
	case TypeSRV:
		buf = binary.BigEndian.AppendUint16(buf, rr.Priority)
		buf = binary.BigEndian.AppendUint16(buf, rr.Weight)
		buf = binary.BigEndian.AppendUint16(buf, rr.Port)
		if buf, err = packName(buf, rr.Target, comp, false); err != nil {
			return nil, err
		}
	case TypeSOA:
		if buf, err = packName(buf, rr.Target, comp, false); err != nil {
			return nil, err
		}
		if buf, err = packName(buf, rr.Rname, comp, false); err != nil {
			return nil, err
		}
		buf = binary.BigEndian.AppendUint32(buf, rr.Serial)
		buf = binary.BigEndian.AppendUint32(buf, rr.Refresh)
		buf = binary.BigEndian.AppendUint32(buf, rr.Retry)
		buf = binary.BigEndian.AppendUint32(buf, rr.Expire)
		buf = binary.BigEndian.AppendUint32(buf, rr.Minimum)
	default:
		buf = append(buf, rr.Raw...)
	}
	binary.BigEndian.PutUint16(buf[lenPos:lenPos+2], uint16(len(buf)-start))
	return buf, nil
}

// String renders the RR in master-file presentation format.
func (rr RR) String() string {
	var rdata string
	switch rr.Type {
	case TypeA, TypeAAAA:
		rdata = rr.IP.String()
	case TypeCNAME, TypeNS, TypePTR:
		rdata = rr.Target
	case TypeMX:
		rdata = fmt.Sprintf("%d %s", rr.Preference, rr.Target)
	case TypeTXT:
		var q []string
		for _, s := range rr.Text {
			q = append(q, fmt.Sprintf("%q", s))
		}
		rdata = strings.Join(q, " ")
	case TypeSRV:
		rdata = fmt.Sprintf("%d %d %d %s", rr.Priority, rr.Weight, rr.Port, rr.Target)
	case TypeSOA:
		rdata = fmt.Sprintf("%s %s %d %d %d %d %d", rr.Target, rr.Rname, rr.Serial, rr.Refresh, rr.Retry, rr.Expire, rr.Minimum)
	default:
		rdata = fmt.Sprintf("\\# %d %x", len(rr.Raw), rr.Raw)
	}
	return fmt.Sprintf("%s %d IN %s %s", rr.Name, rr.TTL, TypeString(rr.Type), rdata)
}
