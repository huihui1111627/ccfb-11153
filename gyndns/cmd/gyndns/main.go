// gyndns is an authoritative DNS server with subnet/weight based traffic
// steering, health-check driven failover and master/slave replication.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gyndns/internal/dnsmsg"
	"gyndns/internal/health"
	"gyndns/internal/qlog"
	"gyndns/internal/router"
	"gyndns/internal/server"
	"gyndns/internal/web"
	"gyndns/internal/xfr"
	"gyndns/internal/zone"
)

func main() {
	var (
		role       = flag.String("role", "master", "master or slave")
		dnsAddr    = flag.String("dns", ":5353", "DNS listen address (udp+tcp)")
		httpAddr   = flag.String("http", ":8053", "admin UI listen address")
		dataFile   = flag.String("data", "gyndns-data.json", "zone data persistence file")
		slaves     = flag.String("slaves", "", "master: comma separated slave DNS addresses for NOTIFY")
		masterAddr = flag.String("master", "", "slave: master DNS address host:port")
		zones      = flag.String("zones", "", "slave: comma separated zones to pull from master")
		zoneFile   = flag.String("zonefile", "", "master: import this zone file when the data file is empty")
		checkInt   = flag.Duration("check-interval", 5*time.Second, "health check interval")
		refreshInt = flag.Duration("refresh-interval", 60*time.Second, "slave: periodic re-transfer interval")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	store := zone.NewStore()
	if err := store.SetPersistFile(*dataFile); err != nil {
		log.Fatalf("load data: %v", err)
	}
	if *role == "slave" {
		store.ReadOnly = true
	}

	// master: import a zone file when starting with an empty store
	if *role == "master" && *zoneFile != "" && len(store.Zones()) == 0 {
		f, err := os.Open(*zoneFile)
		if err != nil {
			log.Fatalf("open zonefile: %v", err)
		}
		z, err := zone.ParseZoneFile(f)
		f.Close()
		if err != nil {
			log.Fatalf("parse zonefile: %v", err)
		}
		store.ReplaceZone(z, true)
		log.Printf("imported zone %s (%d records) from %s", z.Name, len(z.Records), *zoneFile)
	}

	// health checking
	hc := health.New(*checkInt)
	rt := router.New(hc)
	store.Selector = rt
	targets := func() []health.Target {
		var out []health.Target
		for _, zn := range store.Zones() {
			z := store.GetZone(zn)
			for _, r := range z.Records {
				if r.Check != "" && (r.Type == dnsmsg.TypeA || r.Type == dnsmsg.TypeAAAA) {
					out = append(out, health.Target{RecKey: r.Key(), Check: r.Check})
				}
			}
		}
		return out
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hc.Run(ctx, targets)

	// dns server
	ql := qlog.New(10000)
	srv := server.New(store, ql)

	// replication
	var m *xfr.Master
	var sl *xfr.Slave
	if *role == "master" {
		var addrs []string
		for _, a := range strings.Split(*slaves, ",") {
			if a = strings.TrimSpace(a); a != "" {
				addrs = append(addrs, a)
			}
		}
		m = xfr.NewMaster(store, addrs)
		srv.Master = m
		store.OnChange = func(zoneName string, serial uint32) {
			log.Printf("zone %s changed, serial %d, notifying %d slaves", zoneName, serial, len(addrs))
			m.NotifySlaves(zoneName, serial)
		}
	} else {
		if *masterAddr == "" {
			log.Fatal("slave mode needs -master host:port")
		}
		sl = xfr.NewSlave(store, *masterAddr)
		srv.Slave = sl
		zoneList := []string{}
		for _, zn := range strings.Split(*zones, ",") {
			if zn = strings.TrimSpace(zn); zn != "" {
				zoneList = append(zoneList, zone.Normalize(zn))
			}
		}
		// initial transfer, then periodic refresh as a safety net for
		// lost NOTIFY packets
		go func() {
			for _, zn := range zoneList {
				if err := sl.Transfer(zn); err != nil {
					log.Printf("initial transfer %s: %v", zn, err)
				}
			}
			t := time.NewTicker(*refreshInt)
			defer t.Stop()
			for range t.C {
				for _, zn := range zoneList {
					if err := sl.Transfer(zn); err != nil {
						log.Printf("refresh transfer %s: %v", zn, err)
					}
				}
			}
		}()
	}

	go func() {
		log.Printf("DNS UDP listening on %s", *dnsAddr)
		if err := srv.ServeUDP(ctx, *dnsAddr); err != nil && ctx.Err() == nil {
			log.Fatalf("udp: %v", err)
		}
	}()
	go func() {
		log.Printf("DNS TCP listening on %s", *dnsAddr)
		if err := srv.ServeTCP(ctx, *dnsAddr); err != nil && ctx.Err() == nil {
			log.Fatalf("tcp: %v", err)
		}
	}()

	webSrv := web.New(store, ql, hc, *role)
	webSrv.Master = m
	webSrv.Slave = sl
	httpServer := &http.Server{Addr: *httpAddr, Handler: webSrv.Handler()}
	go func() {
		log.Printf("admin UI on http://localhost%s", *httpAddr)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	cancel()
	httpServer.Close()
}
