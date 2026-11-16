package zone

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"gyndns/internal/dnsmsg"
)

// ErrReadOnly is returned when mutating a slave-mode store.
var ErrReadOnly = errors.New("zone store is read-only (slave mode)")

// persist handles JSON persistence of the store.
type persist struct {
	mu   sync.Mutex
	path string
}

type diskFormat struct {
	Zones    map[string]*Zone    `json:"zones"`
	Journals map[string][]Change `json:"journals,omitempty"`
}

// SetPersistFile enables persistence to the given JSON file and loads any
// existing content.
func (s *Store) SetPersistFile(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(path)
	if err == nil {
		var df diskFormat
		if err := json.Unmarshal(data, &df); err != nil {
			return fmt.Errorf("load %s: %w", path, err)
		}
		for name, z := range df.Zones {
			z.Name = Normalize(z.Name)
			for _, r := range z.Records {
				r.Name = Normalize(r.Name)
			}
			z.Index()
			s.zones[name] = z
		}
		if df.Journals != nil {
			s.journals = df.Journals
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// stash the path via a wrapper field
	s.persistPath = path
	return nil
}

// saveLocked persists current state; caller must not hold s.mu.
func (s *Store) save() {
	if s.persistPath == "" {
		return
	}
	s.mu.RLock()
	df := diskFormat{Zones: s.zones, Journals: s.journals}
	s.mu.RUnlock()
	data, err := json.MarshalIndent(df, "", "  ")
	if err != nil {
		return
	}
	tmp := s.persistPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err == nil {
		os.Rename(tmp, s.persistPath)
	}
}

// changedLocked bumps the serial, journals and persists after a mutation.
// Caller holds s.mu (write).
func (s *Store) changedLocked(zoneName string, add, del []Record) {
	z := s.zones[zoneName]
	if z == nil {
		return
	}
	z.BumpSerial()
	z.Index()
	serial := z.Serial()
	j := s.journals[zoneName]
	j = append(j, Change{Serial: serial, Add: add, Del: del})
	if len(j) > s.maxJournal {
		j = j[len(j)-s.maxJournal:]
	}
	s.journals[zoneName] = j
	// persist + notify outside the lock would be nicer; mutations are rare
	// and cheap here, so do it inline but asynchronously for the callback.
	cb := s.OnChange
	go func() {
		s.save()
		if cb != nil {
			cb(zoneName, serial)
		}
	}()
}

// CreateZone adds a new zone with a SOA and NS record.
func (s *Store) CreateZone(name, ns, mbox string) error {
	name = Normalize(name)
	ns = Normalize(ns)
	mbox = Normalize(mbox)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ReadOnly {
		return ErrReadOnly
	}
	if _, ok := s.zones[name]; ok {
		return fmt.Errorf("zone %s already exists", name)
	}
	soa := &dnsmsg.SOA{NS: ns, Mbox: mbox, Serial: 1, Refresh: 300, Retry: 60, Expire: 1209600, MinTTL: 60}
	z := &Zone{
		Name: name,
		Records: []*Record{
			{Name: name, Type: dnsmsg.TypeSOA, TTL: 300, RData: soa.String()},
			{Name: name, Type: dnsmsg.TypeNS, TTL: 300, RData: ns},
		},
	}
	z.Index()
	s.zones[name] = z
	var all []Record
	for _, r := range z.Records {
		all = append(all, *r)
	}
	s.changedLocked(name, all, nil)
	return nil
}

// DeleteZone removes a zone entirely.
func (s *Store) DeleteZone(name string) error {
	name = Normalize(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ReadOnly {
		return ErrReadOnly
	}
	if _, ok := s.zones[name]; !ok {
		return fmt.Errorf("zone %s not found", name)
	}
	delete(s.zones, name)
	delete(s.journals, name)
	go s.save()
	return nil
}

// AddRecord inserts a record into a zone.
func (s *Store) AddRecord(zoneName string, rec Record) error {
	zoneName = Normalize(zoneName)
	rec.Name = Normalize(rec.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ReadOnly {
		return ErrReadOnly
	}
	z := s.zones[zoneName]
	if z == nil {
		return fmt.Errorf("zone %s not found", zoneName)
	}
	if rec.TTL == 0 {
		rec.TTL = 300
	}
	if _, err := rec.RR(""); err != nil {
		return fmt.Errorf("bad rdata: %w", err)
	}
	r := rec
	z.Records = append(z.Records, &r)
	s.changedLocked(zoneName, []Record{rec}, nil)
	return nil
}

// DeleteRecord removes records matching name+type+rdata (empty rdata = all of
// that name+type).
func (s *Store) DeleteRecord(zoneName, name string, rtype uint16, rdata string) error {
	zoneName = Normalize(zoneName)
	name = Normalize(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ReadOnly {
		return ErrReadOnly
	}
	z := s.zones[zoneName]
	if z == nil {
		return fmt.Errorf("zone %s not found", zoneName)
	}
	var kept []*Record
	var removed []Record
	for _, r := range z.Records {
		if r.Name == name && r.Type == rtype && (rdata == "" || r.RData == rdata) {
			removed = append(removed, *r)
			continue
		}
		kept = append(kept, r)
	}
	if len(removed) == 0 {
		return fmt.Errorf("record not found")
	}
	z.Records = kept
	s.changedLocked(zoneName, nil, removed)
	return nil
}

// UpdateRecord replaces old (name+type+rdata) with rec.
func (s *Store) UpdateRecord(zoneName, name string, rtype uint16, rdata string, rec Record) error {
	zoneName = Normalize(zoneName)
	name = Normalize(name)
	rec.Name = Normalize(rec.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ReadOnly {
		return ErrReadOnly
	}
	z := s.zones[zoneName]
	if z == nil {
		return fmt.Errorf("zone %s not found", zoneName)
	}
	if _, err := rec.RR(""); err != nil {
		return fmt.Errorf("bad rdata: %w", err)
	}
	found := false
	var removed []Record
	for _, r := range z.Records {
		if r.Name == name && r.Type == rtype && r.RData == rdata {
			removed = append(removed, *r)
			*r = rec
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("record not found")
	}
	s.changedLocked(zoneName, []Record{rec}, removed)
	return nil
}

// ReplaceZone atomically installs a full zone (zone file import / AXFR).
// Journals one change carrying the whole new content.
func (s *Store) ReplaceZone(z *Zone, journal bool) {
	z.Name = Normalize(z.Name)
	for _, r := range z.Records {
		r.Name = Normalize(r.Name)
	}
	z.Index()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.zones[z.Name] = z
	if journal && !s.ReadOnly {
		var all []Record
		for _, r := range z.Records {
			all = append(all, *r)
		}
		s.changedLocked(z.Name, all, nil)
	} else {
		go s.save()
	}
}

// ApplyIXFR applies a sequence of journal changes (slave side).
func (s *Store) ApplyIXFR(zoneName string, changes []Change) error {
	zoneName = Normalize(zoneName)
	s.mu.Lock()
	defer s.mu.Unlock()
	z := s.zones[zoneName]
	if z == nil {
		return fmt.Errorf("zone %s not found locally", zoneName)
	}
	for _, ch := range changes {
		for _, d := range ch.Del {
			del := NormalizeRecord(d)
			for i, r := range z.Records {
				if r.Name == del.Name && r.Type == del.Type && r.RData == del.RData {
					z.Records = append(z.Records[:i], z.Records[i+1:]...)
					break
				}
			}
		}
		for _, a := range ch.Add {
			add := NormalizeRecord(a)
			dup := false
			for _, r := range z.Records {
				if r.Name == add.Name && r.Type == add.Type && r.RData == add.RData {
					dup = true
					break
				}
			}
			if !dup {
				r := add
				z.Records = append(z.Records, &r)
			}
		}
		// sync SOA serial
		if soa := z.SOA(); soa != nil {
			if d, err := dnsmsg.ParseRDataString(dnsmsg.TypeSOA, soa.RData); err == nil {
				so := d.(*dnsmsg.SOA)
				so.Serial = ch.Serial
				soa.RData = so.String()
			}
		}
	}
	z.Index()
	go s.save()
	return nil
}

// NormalizeRecord normalizes the owner name of a record value.
func NormalizeRecord(r Record) Record {
	r.Name = Normalize(r.Name)
	return r
}

// SortedRecords returns the zone's records in display order.
func (z *Zone) SortedRecords() []*Record {
	out := make([]*Record, len(z.Records))
	copy(out, z.Records)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].RData < out[j].RData
	})
	return out
}

// LoadPersist is a convenience for tests.
func (s *Store) LoadPersist(dir string) error {
	return s.SetPersistFile(filepath.Join(dir, "zones.json"))
}
