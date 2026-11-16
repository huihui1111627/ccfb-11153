// Package xfr implements master/slave replication: NOTIFY on change,
// IXFR from the change journal, AXFR fallback when the journal cannot
// cover the slave's serial.
package xfr

import (
	"fmt"
	"net"
	"sync"
	"time"

	"gyndns/internal/dnsmsg"
	"gyndns/internal/zone"
)

// Master serves zone transfers and notifies slaves of changes.
type Master struct {
	Store  *zone.Store
	Slaves []string // DNS addresses host:port of slaves to NOTIFY

	mu        sync.Mutex
	lastNotif map[string]time.Time
}

// NewMaster creates the master-side sync state.
func NewMaster(store *zone.Store, slaves []string) *Master {
	return &Master{Store: store, Slaves: slaves, lastNotif: map[string]time.Time{}}
}

// NotifySlaves sends a DNS NOTIFY for the zone to every configured slave.
func (m *Master) NotifySlaves(zoneName string, serial uint32) {
	m.mu.Lock()
	if t, ok := m.lastNotif[zoneName]; ok && time.Since(t) < 500*time.Millisecond {
		m.mu.Unlock()
		return // debounce bursts of edits
	}
	m.lastNotif[zoneName] = time.Now()
	m.mu.Unlock()

	for _, addr := range m.Slaves {
		go func(addr string) {
			msg := &dnsmsg.Message{
				Header:    dnsmsg.Header{ID: uint16(time.Now().UnixNano()), Opcode: dnsmsg.OpcodeNotify, AA: true},
				Questions: []dnsmsg.Question{{Name: zoneName, Type: dnsmsg.TypeSOA, Class: dnsmsg.ClassIN}},
			}
			buf, err := msg.Pack(0)
			if err != nil {
				return
			}
			conn, err := net.DialTimeout("udp", addr, 2*time.Second)
			if err != nil {
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Write(buf); err != nil {
				return
			}
			resp := make([]byte, 512)
			if _, err := conn.Read(resp); err != nil {
				return // best effort
			}
		}(addr)
	}
}

// HandleTransfer answers an AXFR/IXFR query (TCP only). Returns nil if the
// query is not a transfer request.
func (m *Master) HandleTransfer(q *dnsmsg.Message) *dnsmsg.Message {
	if len(q.Questions) == 0 {
		return nil
	}
	qu := q.Questions[0]
	if qu.Type != dnsmsg.TypeAXFR && qu.Type != dnsmsg.TypeIXFR {
		return nil
	}
	zoneName := zone.Normalize(qu.Name)
	reply := dnsmsg.NewReply(q)
	z := m.Store.GetZone(zoneName)
	if z == nil {
		reply.RCode = dnsmsg.RcodeRefused
		return reply
	}
	soaRec := z.SOA()
	if soaRec == nil {
		reply.RCode = dnsmsg.RcodeServerFailure
		return reply
	}
	soaRR, err := soaRec.RR("")
	if err != nil {
		reply.RCode = dnsmsg.RcodeServerFailure
		return reply
	}
	curSerial := z.Serial()

	if qu.Type == dnsmsg.TypeIXFR {
		slaveSerial := uint32(0)
		for _, rr := range q.Authorities {
			if rr.Type == dnsmsg.TypeSOA {
				if soa, ok := rr.Data.(*dnsmsg.SOA); ok {
					slaveSerial = soa.Serial
				}
			}
		}
		if slaveSerial >= curSerial {
			// up to date: single SOA
			reply.Answers = []dnsmsg.RR{soaRR}
			return reply
		}
		// find journal coverage
		journal := m.Store.Journal(zoneName)
		start := -1
		for i, ch := range journal {
			if ch.Serial > slaveSerial {
				start = i
				break
			}
		}
		// journal covers the gap only if the first needed change's
		// predecessor serial == slaveSerial (or journal starts exactly
		// after it)
		covers := start >= 0 && (start == 0 && journal[0].Serial == slaveSerial+1 ||
			start > 0 && journal[start-1].Serial == slaveSerial)
		if covers {
			reply.Answers = append(reply.Answers, soaRR)
			prev := slaveSerial
			for _, ch := range journal[start:] {
				old := *soaRR.Data.(*dnsmsg.SOA)
				old.Serial = prev
				reply.Answers = append(reply.Answers, dnsmsg.RR{
					Name: zoneName, Type: dnsmsg.TypeSOA, Class: dnsmsg.ClassIN,
					TTL: soaRR.TTL, Data: &old,
				})
				for _, d := range ch.Del {
					if rr, err := d.RR(""); err == nil {
						reply.Answers = append(reply.Answers, rr)
					}
				}
				nw := *soaRR.Data.(*dnsmsg.SOA)
				nw.Serial = ch.Serial
				reply.Answers = append(reply.Answers, dnsmsg.RR{
					Name: zoneName, Type: dnsmsg.TypeSOA, Class: dnsmsg.ClassIN,
					TTL: soaRR.TTL, Data: &nw,
				})
				for _, a := range ch.Add {
					if rr, err := a.RR(""); err == nil {
						reply.Answers = append(reply.Answers, rr)
					}
				}
				prev = ch.Serial
			}
			reply.Answers = append(reply.Answers, soaRR)
			return reply
		}
		// journal cannot cover: fall through to full AXFR-style answer
	}

	// AXFR: SOA, all records, SOA
	reply.Answers = append(reply.Answers, soaRR)
	for _, r := range z.Records {
		if r.Type == dnsmsg.TypeSOA {
			continue
		}
		if rr, err := r.RR(""); err == nil {
			reply.Answers = append(reply.Answers, rr)
		}
	}
	reply.Answers = append(reply.Answers, soaRR)
	return reply
}

// SlaveStatus describes one zone's sync state on a slave.
type SlaveStatus struct {
	Zone       string    `json:"zone"`
	Serial     uint32    `json:"serial"`
	LastSync   time.Time `json:"last_sync"`
	LastResult string    `json:"last_result"`
}

// Slave pulls zone transfers from a master.
type Slave struct {
	Store      *zone.Store
	MasterAddr string // master DNS address host:port

	mu     sync.Mutex
	status map[string]*SlaveStatus
	// OnSyncDone is called after a successful transfer (e.g. to persist).
	transferring map[string]bool
}

// NewSlave creates the slave-side sync state.
func NewSlave(store *zone.Store, masterAddr string) *Slave {
	return &Slave{
		Store:        store,
		MasterAddr:   masterAddr,
		status:       map[string]*SlaveStatus{},
		transferring: map[string]bool{},
	}
}

// Status returns a snapshot of all zone sync states.
func (s *Slave) Status() []SlaveStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SlaveStatus, 0, len(s.status))
	for _, st := range s.status {
		out = append(out, *st)
	}
	return out
}

func (s *Slave) setStatus(zoneName, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.status[zoneName]
	if !ok {
		st = &SlaveStatus{Zone: zoneName}
		s.status[zoneName] = st
	}
	st.LastSync = time.Now()
	st.LastResult = result
	if z := s.Store.GetZone(zoneName); z != nil {
		st.Serial = z.Serial()
	}
}

// HandleNotify processes a NOTIFY from the master and triggers a transfer.
// Returns the NOTIFY response.
func (s *Slave) HandleNotify(q *dnsmsg.Message) *dnsmsg.Message {
	reply := &dnsmsg.Message{
		Header:    dnsmsg.Header{ID: q.ID, QR: true, Opcode: dnsmsg.OpcodeNotify},
		Questions: q.Questions,
	}
	if len(q.Questions) == 0 {
		reply.RCode = dnsmsg.RcodeFormatError
		return reply
	}
	zoneName := zone.Normalize(q.Questions[0].Name)
	go func() {
		if err := s.Transfer(zoneName); err != nil {
			s.setStatus(zoneName, "transfer failed: "+err.Error())
		}
	}()
	return reply
}

// Transfer performs one IXFR (falling back to AXFR as decided by the
// master) for the zone.
func (s *Slave) Transfer(zoneName string) error {
	zoneName = zone.Normalize(zoneName)
	s.mu.Lock()
	if s.transferring[zoneName] {
		s.mu.Unlock()
		return nil
	}
	s.transferring[zoneName] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.transferring, zoneName)
		s.mu.Unlock()
	}()

	var serial uint32
	if z := s.Store.GetZone(zoneName); z != nil {
		serial = z.Serial()
	}
	q := &dnsmsg.Message{
		Header:    dnsmsg.Header{ID: uint16(time.Now().UnixNano()), RD: false},
		Questions: []dnsmsg.Question{{Name: zoneName, Type: dnsmsg.TypeIXFR, Class: dnsmsg.ClassIN}},
	}
	// carry our current serial in the authority section per RFC 1995
	soaData := &dnsmsg.SOA{NS: "slave.", Mbox: "root.slave.", Serial: serial}
	q.Authorities = []dnsmsg.RR{{
		Name: zoneName, Type: dnsmsg.TypeSOA, Class: dnsmsg.ClassNone,
		TTL: 0, Data: soaData,
	}}
	buf, err := q.Pack(0)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", s.MasterAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect master: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	var lbuf [2]byte
	lbuf[0] = byte(len(buf) >> 8)
	lbuf[1] = byte(len(buf))
	if _, err := conn.Write(append(lbuf[:], buf...)); err != nil {
		return err
	}
	if _, err := readFull(conn, lbuf[:]); err != nil {
		return err
	}
	rlen := int(lbuf[0])<<8 | int(lbuf[1])
	rbuf := make([]byte, rlen)
	if _, err := readFull(conn, rbuf); err != nil {
		return err
	}
	resp, err := dnsmsg.Unpack(rbuf)
	if err != nil {
		return fmt.Errorf("bad transfer response: %w", err)
	}
	if resp.RCode != dnsmsg.RcodeSuccess {
		return fmt.Errorf("transfer refused: %s", dnsmsg.RcodeString(resp.RCode))
	}
	if len(resp.Answers) == 0 {
		return fmt.Errorf("empty transfer response")
	}
	first, ok := resp.Answers[0].Data.(*dnsmsg.SOA)
	if !ok {
		return fmt.Errorf("transfer response does not start with SOA")
	}
	if len(resp.Answers) == 1 {
		s.setStatus(zoneName, fmt.Sprintf("已是最新 (serial %d)", serial))
		return nil
	}

	if _, isSOA := resp.Answers[1].Data.(*dnsmsg.SOA); isSOA {
		// IXFR sequence
		changes, err := parseIXFR(resp.Answers)
		if err != nil {
			return err
		}
		if err := s.Store.ApplyIXFR(zoneName, changes); err != nil {
			return err
		}
		s.setStatus(zoneName, fmt.Sprintf("增量同步 %d 个变更, serial %d -> %d", len(changes), serial, first.Serial))
		return nil
	}
	// AXFR: rebuild the zone from scratch
	nz := &zone.Zone{Name: zoneName}
	// answers = SOA, records..., SOA — keep everything but the trailing SOA
	for _, rr := range resp.Answers[:len(resp.Answers)-1] {
		rec := zone.Record{
			Name:  rr.Name,
			Type:  rr.Type,
			TTL:   rr.TTL,
			RData: rr.Data.String(),
		}
		nz.Records = append(nz.Records, &rec)
	}
	// routing metadata is not transferred over DNS; keep existing metadata
	// for records that still exist
	if old := s.Store.GetZone(zoneName); old != nil {
		meta := map[string]zone.Record{}
		for _, r := range old.Records {
			meta[r.Key()] = *r
		}
		for _, r := range nz.Records {
			if m, ok := meta[r.Key()]; ok {
				r.Weight, r.Subnets, r.Backup, r.Check = m.Weight, m.Subnets, m.Backup, m.Check
			}
		}
	}
	s.Store.ReplaceZone(nz, false)
	s.setStatus(zoneName, fmt.Sprintf("全量同步 %d 条记录, serial %d", len(nz.Records), first.Serial))
	return nil
}

// parseIXFR decodes an RFC 1995 delta sequence into journal changes.
func parseIXFR(ans []dnsmsg.RR) ([]zone.Change, error) {
	// ans[0] and ans[len-1] are the final SOA; between them:
	// SOA(old) del... SOA(new) add... (repeated)
	var changes []zone.Change
	i := 1
	for i < len(ans)-1 {
		oldSOA, ok := ans[i].Data.(*dnsmsg.SOA)
		if !ok {
			return nil, fmt.Errorf("ixfr: expected SOA at position %d", i)
		}
		_ = oldSOA
		i++
		ch := zone.Change{}
		for i < len(ans) && ans[i].Type != dnsmsg.TypeSOA {
			ch.Del = append(ch.Del, zone.Record{
				Name: ans[i].Name, Type: ans[i].Type, TTL: ans[i].TTL,
				RData: ans[i].Data.String(),
			})
			i++
		}
		if i >= len(ans) {
			return nil, fmt.Errorf("ixfr: missing new SOA")
		}
		newSOA := ans[i].Data.(*dnsmsg.SOA)
		ch.Serial = newSOA.Serial
		i++
		for i < len(ans) && ans[i].Type != dnsmsg.TypeSOA {
			ch.Add = append(ch.Add, zone.Record{
				Name: ans[i].Name, Type: ans[i].Type, TTL: ans[i].TTL,
				RData: ans[i].Data.String(),
			})
			i++
		}
		changes = append(changes, ch)
	}
	return changes, nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := conn.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
