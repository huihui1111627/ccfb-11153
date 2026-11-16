// Package dnsmsg implements DNS message encoding and decoding (RFC 1035,
// RFC 3597, EDNS0 per RFC 6891, ECS per RFC 7871) from scratch.
package dnsmsg

import "fmt"

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
	TypeIXFR  uint16 = 251
	TypeAXFR  uint16 = 252
	TypeANY   uint16 = 255
)

// Classes.
const (
	ClassIN   uint16 = 1
	ClassNone uint16 = 254
	ClassANY  uint16 = 255
)

// Rcodes.
const (
	RcodeSuccess        uint8 = 0
	RcodeFormatError    uint8 = 1
	RcodeServerFailure  uint8 = 2
	RcodeNameError      uint8 = 3
	RcodeNotImplemented uint8 = 4
	RcodeRefused        uint8 = 5
)

// Opcodes.
const (
	OpcodeQuery  uint8 = 0
	OpcodeStatus uint8 = 2
	OpcodeNotify uint8 = 4
	OpcodeUpdate uint8 = 5
)

// EDNS option codes.
const (
	EDNSOptionECS uint16 = 8
)

var typeNames = map[uint16]string{
	TypeA: "A", TypeNS: "NS", TypeCNAME: "CNAME", TypeSOA: "SOA",
	TypePTR: "PTR", TypeMX: "MX", TypeTXT: "TXT", TypeAAAA: "AAAA",
	TypeSRV: "SRV", TypeOPT: "OPT", TypeIXFR: "IXFR", TypeAXFR: "AXFR",
	TypeANY: "ANY",
}

var nameTypes map[string]uint16

func init() {
	nameTypes = make(map[string]uint16, len(typeNames))
	for t, n := range typeNames {
		nameTypes[n] = t
	}
}

// TypeString returns the mnemonic for a record type.
func TypeString(t uint16) string {
	if s, ok := typeNames[t]; ok {
		return s
	}
	return fmt.Sprintf("TYPE%d", t)
}

// StringToType parses a type mnemonic. Returns false for unknown names.
func StringToType(s string) (uint16, bool) {
	t, ok := nameTypes[s]
	return t, ok
}

// RcodeString returns a human readable rcode name.
func RcodeString(r uint8) string {
	switch r {
	case RcodeSuccess:
		return "NOERROR"
	case RcodeFormatError:
		return "FORMERR"
	case RcodeServerFailure:
		return "SERVFAIL"
	case RcodeNameError:
		return "NXDOMAIN"
	case RcodeNotImplemented:
		return "NOTIMP"
	case RcodeRefused:
		return "REFUSED"
	}
	return fmt.Sprintf("RCODE%d", r)
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
	RCode  uint8
}

// Question is a DNS question section entry.
type Question struct {
	Name  string
	Type  uint16
	Class uint16
}

// Message is a full DNS message.
type Message struct {
	Header
	Questions   []Question
	Answers     []RR
	Authorities []RR
	Additionals []RR
}

// NewReply builds a response skeleton mirroring the query's ID/RD and questions.
func NewReply(q *Message) *Message {
	return &Message{
		Header: Header{
			ID:     q.ID,
			QR:     true,
			Opcode: q.Opcode,
			RD:     q.RD,
			AA:     true,
		},
		Questions: q.Questions,
	}
}

// UDPSize returns the requestor's UDP payload size: the EDNS advertised size,
// or 512 when the query carries no OPT record.
func (m *Message) UDPSize() int {
	for _, rr := range m.Additionals {
		if rr.Type == TypeOPT {
			if int(rr.Class) < 512 {
				return 512
			}
			return int(rr.Class)
		}
	}
	return 512
}

// HasEDNS reports whether the message carries an OPT pseudo-RR.
func (m *Message) HasEDNS() bool {
	for _, rr := range m.Additionals {
		if rr.Type == TypeOPT {
			return true
		}
	}
	return false
}

// SetEDNS adds (or replaces) the OPT record advertising the given UDP size.
func (m *Message) SetEDNS(size uint16) {
	out := m.Additionals[:0]
	for _, rr := range m.Additionals {
		if rr.Type != TypeOPT {
			out = append(out, rr)
		}
	}
	m.Additionals = append(out, RR{
		Name:  ".",
		Type:  TypeOPT,
		Class: size,
		Data:  &OPT{},
	})
}
