package router

import (
	"fmt"
	"net"
	"testing"

	"gyndns/internal/dnsmsg"
	"gyndns/internal/zone"
)

type fakeHealth map[string]bool

func (f fakeHealth) Up(key string) bool { return f[key] }

func rec(rdata string, weight int, subnets []string, backup bool, check string) *zone.Record {
	return &zone.Record{
		Name: "app.gym.example.", Type: dnsmsg.TypeA, TTL: 60,
		RData: rdata, Weight: weight, Subnets: subnets, Backup: backup, Check: check,
	}
}

func TestSubnetSelection(t *testing.T) {
	r := New(nil)
	cands := []*zone.Record{
		rec("10.1.0.10", 0, []string{"10.1.0.0/16"}, false, ""),
		rec("10.2.0.10", 0, []string{"10.2.0.0/16"}, false, ""),
		rec("203.0.113.1", 0, nil, false, ""), // global
	}
	out := r.Select(cands, net.ParseIP("10.1.5.5"), func(string, ...any) {})
	if len(out) != 1 || out[0].RData != "10.1.0.10" {
		t.Fatalf("subnet match: %+v", out)
	}
	// unknown subnet falls into the global pool
	out = r.Select(cands, net.ParseIP("172.16.0.1"), func(string, ...any) {})
	if len(out) != 1 || out[0].RData != "203.0.113.1" {
		t.Fatalf("global fallback: %+v", out)
	}
}

func TestHealthFilteringAndBackup(t *testing.T) {
	h := fakeHealth{}
	cands := []*zone.Record{
		rec("10.1.0.10", 0, nil, false, "tcp://10.1.0.10:80"),
		rec("10.2.0.10", 0, nil, false, "tcp://10.2.0.10:80"),
		rec("203.0.113.9", 0, nil, true, ""), // backup
	}
	k1, k2 := cands[0].Key(), cands[1].Key()
	h[k1], h[k2] = true, true
	r := New(h)
	out := r.Select(cands, nil, func(string, ...any) {})
	if out[0].RData == "203.0.113.9" {
		t.Fatal("backup used while healthy records exist")
	}
	// one down
	h[k1] = false
	for i := 0; i < 20; i++ {
		out = r.Select(cands, nil, func(string, ...any) {})
		if out[0].RData != "10.2.0.10" {
			t.Fatalf("unhealthy record answered: %s", out[0].RData)
		}
	}
	// all down -> backup
	h[k2] = false
	out = r.Select(cands, nil, func(string, ...any) {})
	if len(out) != 1 || out[0].RData != "203.0.113.9" {
		t.Fatalf("backup not used when all down: %+v", out)
	}
}

func TestWeightDistribution(t *testing.T) {
	r := New(nil)
	cands := []*zone.Record{
		rec("10.0.0.1", 90, nil, false, ""),
		rec("10.0.0.2", 10, nil, false, ""),
	}
	counts := map[string]int{}
	for i := 0; i < 10000; i++ {
		out := r.Select(cands, nil, func(string, ...any) {})
		counts[out[0].RData]++
	}
	ratio := float64(counts["10.0.0.1"]) / float64(counts["10.0.0.2"])
	if ratio < 6 || ratio > 13 {
		t.Fatalf("weight ratio off: %v (%v)", counts, ratio)
	}
}

func TestTraceWritten(t *testing.T) {
	h := fakeHealth{}
	c := rec("10.1.0.10", 0, nil, false, "tcp://x")
	h[c.Key()] = false
	r := New(h)
	var traces []string
	r.Select([]*zone.Record{c}, nil, func(f string, a ...any) {
		traces = append(traces, fmt.Sprintf(f, a...))
	})
	if len(traces) == 0 {
		t.Fatal("expected decision trace")
	}
}
