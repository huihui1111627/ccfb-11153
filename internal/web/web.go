// Package web exposes the management HTTP API and serves the admin UI.
package web

import (
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"dnsd/internal/geo"
	"dnsd/internal/health"
	"dnsd/internal/ratelimit"
	"dnsd/internal/stats"
	"dnsd/internal/zone"
)

//go:embed static
var staticFS embed.FS

// Deps are the components the API manipulates.
type Deps struct {
	Store  *zone.Store
	Health *health.Checker
	RL     *ratelimit.Limiter
	Stats  *stats.Stats
	Geo    *geo.Matcher
}

// Handler returns the root HTTP handler.
func Handler(d *Deps) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	mux.HandleFunc("/api/zones", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			type summary struct {
				Name    string `json:"name"`
				Records int    `json:"records"`
				Serial  uint32 `json:"serial"`
			}
			var out []summary
			for _, z := range d.Store.List() {
				n := 0
				for _, rs := range z.Records {
					n += len(rs)
				}
				out = append(out, summary{z.Name, n, z.SOA.Serial})
			}
			writeJSON(w, out)
		case http.MethodPost:
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
				http.Error(w, "need {\"name\": \"example.com\"}", 400)
				return
			}
			d.Store.AddZone(zone.NewZone(body.Name))
			writeJSON(w, map[string]string{"status": "ok"})
		default:
			http.Error(w, "method not allowed", 405)
		}
	})

	mux.HandleFunc("/api/zones/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/zones/")
		parts := strings.SplitN(rest, "/", 2)
		zname := parts[0]
		sub := ""
		if len(parts) == 2 {
			sub = parts[1]
		}
		switch sub {
		case "":
			switch r.Method {
			case http.MethodGet:
				z := d.Store.Get(zname)
				if z == nil {
					http.Error(w, "zone not found", 404)
					return
				}
				writeJSON(w, z)
			case http.MethodDelete:
				if !d.Store.RemoveZone(zname) {
					http.Error(w, "zone not found", 404)
					return
				}
				writeJSON(w, map[string]string{"status": "ok"})
			default:
				http.Error(w, "method not allowed", 405)
			}
		case "settings":
			if r.Method != http.MethodPut {
				http.Error(w, "method not allowed", 405)
				return
			}
			z := d.Store.Get(zname)
			if z == nil {
				http.Error(w, "zone not found", 404)
				return
			}
			var body struct {
				DefaultTTL uint32         `json:"default_ttl"`
				SOA        zone.SOA       `json:"soa"`
				Fallback4  string         `json:"fallback4"`
				Fallback6  string         `json:"fallback6"`
				Health     zone.HealthCfg `json:"health"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if body.DefaultTTL > 0 {
				z.DefaultTTL = body.DefaultTTL
			}
			z.SOA = body.SOA
			z.Fallback4 = body.Fallback4
			z.Fallback6 = body.Fallback6
			z.Health = body.Health
			z.Touch()
			d.Store.AddZone(z)
			writeJSON(w, map[string]string{"status": "ok"})
		case "export":
			z := d.Store.Get(zname)
			if z == nil {
				http.Error(w, "zone not found", 404)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte(zone.ExportMaster(z)))
		case "import":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", 405)
				return
			}
			data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			z, err := zone.ParseMaster(string(data))
			if err != nil {
				http.Error(w, "parse error: "+err.Error(), 400)
				return
			}
			// Preserve runtime settings from the existing zone if present.
			if old := d.Store.Get(z.Name); old != nil {
				z.Fallback4, z.Fallback6 = old.Fallback4, old.Fallback6
				z.Health = old.Health
			}
			d.Store.AddZone(z)
			writeJSON(w, map[string]string{"status": "ok", "zone": z.Name})
		case "records":
			d.handleRecords(w, r, zname)
		default:
			http.Error(w, "not found", 404)
		}
	})

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.Health.Snapshot())
	})
	mux.HandleFunc("/api/health/probe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		go d.Health.ProbeNow()
		writeJSON(w, map[string]string{"status": "probing"})
	})

	mux.HandleFunc("/api/ratelimit", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, d.RL.Policy())
		case http.MethodPut:
			var p ratelimit.Policy
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			d.RL.SetPolicy(p)
			writeJSON(w, map[string]string{"status": "ok"})
		default:
			http.Error(w, "method not allowed", 405)
		}
	})

	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n <= 0 {
			n = 200
		}
		writeJSON(w, d.Stats.Logs(n))
	})

	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"total":       d.Stats.Total(),
			"assignments": d.Stats.Assignments(),
			"regions":     d.Stats.ByRegion(),
		})
	})

	mux.HandleFunc("/api/geo", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.Geo.Rules())
	})

	return mux
}

func (d *Deps) handleRecords(w http.ResponseWriter, r *http.Request, zname string) {
	z := d.Store.Get(zname)
	if z == nil {
		http.Error(w, "zone not found", 404)
		return
	}
	switch r.Method {
	case http.MethodPost:
		var rec zone.Record
		if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := d.Store.AddRecord(zname, rec); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	case http.MethodPut:
		var body struct {
			Name   string      `json:"name"`
			Type   string      `json:"type"`
			Idx    int         `json:"idx"`
			Record zone.Record `json:"record"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := d.Store.UpdateRecord(zname, body.Name, body.Type, body.Idx, body.Record); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	case http.MethodDelete:
		q := r.URL.Query()
		idx, _ := strconv.Atoi(q.Get("idx"))
		if err := d.Store.DeleteRecord(zname, q.Get("name"), q.Get("type"), idx); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}
