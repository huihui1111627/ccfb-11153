// Command dnsd is an authoritative DNS server with geo/weighted routing,
// active health checks, rate limiting and a web management UI.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dnsd/internal/alias"
	"dnsd/internal/geo"
	"dnsd/internal/health"
	"dnsd/internal/ratelimit"
	"dnsd/internal/server"
	"dnsd/internal/stats"
	"dnsd/internal/web"
	"dnsd/internal/zone"
)

// Datacenter describes one site for weighted traffic split.
type Datacenter struct {
	Name   string `json:"name"`
	Region string `json:"region"`
	Weight int    `json:"weight"`
}

// Config is the daemon configuration file.
type Config struct {
	DNSAddr     string            `json:"dns_addr"`
	HTTPAddr    string            `json:"http_addr"`
	Upstreams   []string          `json:"upstreams"`
	DataDir     string            `json:"data_dir"`
	Datacenters []Datacenter      `json:"datacenters"`
	Subnets     []geo.SubnetRule  `json:"subnets"`
	RateLimit   ratelimit.Policy  `json:"rate_limit"`
	Health      struct {
		IntervalSec int `json:"interval_sec"`
		TimeoutSec  int `json:"timeout_sec"`
	} `json:"health"`
}

func main() {
	cfgPath := flag.String("config", "config.json", "config file path")
	flag.Parse()

	cfg := Config{
		DNSAddr:  ":5354",
		HTTPAddr: ":8054",
		DataDir:  "data",
		Upstreams: []string{"223.5.5.5:53", "119.29.29.29:53"},
		RateLimit: ratelimit.DefaultPolicy(),
	}
	if data, err := os.ReadFile(*cfgPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			log.Fatalf("config: %v", err)
		}
	} else if !os.IsNotExist(err) {
		log.Fatalf("config: %v", err)
	} else {
		log.Printf("config %s not found, using defaults", *cfgPath)
	}

	zoneDir := filepath.Join(cfg.DataDir, "zones")
	os.MkdirAll(zoneDir, 0o755)

	hc := health.New(
		time.Duration(max(cfg.Health.IntervalSec, 1))*time.Second,
		time.Duration(max(cfg.Health.TimeoutSec, 1))*time.Second,
		func(ip string, healthy bool) {
			state := "DOWN"
			if healthy {
				state = "UP"
			}
			log.Printf("health: %s -> %s", ip, state)
		},
	)

	var store *zone.Store
	syncHealth := func() {
		var targets []health.Target
		for _, z := range store.List() {
			if !z.Health.Enabled {
				continue
			}
			for name, recs := range z.Records {
				for _, r := range recs {
					if r.Type != "A" && r.Type != "AAAA" {
						continue
					}
					targets = append(targets, health.Target{
						IP: r.Value, Type: z.Health.Type, Port: z.Health.Port,
						Path: z.Health.Path, Zone: z.Name, Record: name, DC: r.DC,
					})
				}
			}
		}
		hc.SetTargets(targets)
	}
	persist := func(z *zone.Zone) {
		path := filepath.Join(zoneDir, z.Name+".zone")
		if err := os.WriteFile(path, []byte(zone.ExportMaster(z)), 0o644); err != nil {
			log.Printf("persist %s: %v", path, err)
		}
		syncHealth()
	}
	store = zone.NewStore(persist)

	// Load zones from the data directory.
	entries, _ := os.ReadDir(zoneDir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".zone") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(zoneDir, e.Name()))
		if err != nil {
			continue
		}
		z, err := zone.ParseMaster(string(data))
		if err != nil {
			log.Printf("load zone %s: %v", e.Name(), err)
			continue
		}
		store.AddZone(z)
		log.Printf("loaded zone %s (%d names)", z.Name, len(z.Records))
	}
	syncHealth()

	matcher := geo.NewMatcher(geo.NormalizeRuleList(cfg.Subnets))
	rl := ratelimit.New(cfg.RateLimit)
	st := stats.New(2000)
	resolver := alias.New(cfg.Upstreams, 2*time.Second)

	srv := server.New(store, matcher, hc, resolver, rl, st)
	for _, dc := range cfg.Datacenters {
		srv.DCWeights[dc.Name] = dc.Weight
	}

	stop := make(chan struct{})
	go hc.Run()
	go rl.GC(stop)
	go func() {
		if err := srv.ListenAndServe(cfg.DNSAddr, stop); err != nil {
			log.Fatalf("dns: %v", err)
		}
	}()

	deps := &web.Deps{Store: store, Health: hc, RL: rl, Stats: st, Geo: matcher}
	go func() {
		log.Printf("http: management UI on http://%s", cfg.HTTPAddr)
		if err := http.ListenAndServe(cfg.HTTPAddr, web.Handler(deps)); err != nil {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
	close(stop)
	hc.Stop()
}
