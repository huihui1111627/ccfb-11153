// Package web serves the admin UI: zones, records, health checks,
// query logs and master/slave sync status.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"

	"gyndns/internal/dnsmsg"
	"gyndns/internal/health"
	"gyndns/internal/qlog"
	"gyndns/internal/xfr"
	"gyndns/internal/zone"
)

//go:embed templates
var tplFS embed.FS

// Server bundles the dependencies the UI needs.
type Server struct {
	Store  *zone.Store
	Log    *qlog.Log
	Health *health.Checker
	Master *xfr.Master
	Slave  *xfr.Slave
	Role   string // "master" or "slave"

	tpl *template.Template
}

// New creates the web server.
func New(store *zone.Store, log *qlog.Log, hc *health.Checker, role string) *Server {
	funcs := template.FuncMap{
		"typestr": dnsmsg.TypeString,
		"json": func(v any) string {
			b, _ := json.Marshal(v)
			return string(b)
		},
	}
	tpl := template.Must(template.New("").Funcs(funcs).ParseFS(tplFS, "templates/*.html"))
	return &Server{Store: store, Log: log, Health: hc, Role: role, tpl: tpl}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// pages
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/zones", http.StatusFound)
	})
	mux.HandleFunc("GET /zones", s.page("zones"))
	mux.HandleFunc("GET /records", s.page("records"))
	mux.HandleFunc("GET /health", s.page("health"))
	mux.HandleFunc("GET /logs", s.page("logs"))
	mux.HandleFunc("GET /sync", s.page("sync"))
	// api
	mux.HandleFunc("GET /api/zones", s.apiZones)
	mux.HandleFunc("POST /api/zones", s.apiCreateZone)
	mux.HandleFunc("DELETE /api/zones/{name}", s.apiDeleteZone)
	mux.HandleFunc("GET /api/zones/{name}/records", s.apiRecords)
	mux.HandleFunc("POST /api/zones/{name}/records", s.apiAddRecord)
	mux.HandleFunc("PUT /api/zones/{name}/records", s.apiUpdateRecord)
	mux.HandleFunc("DELETE /api/zones/{name}/records", s.apiDeleteRecord)
	mux.HandleFunc("GET /api/zones/{name}/export", s.apiExport)
	mux.HandleFunc("POST /api/zones/{name}/import", s.apiImport)
	mux.HandleFunc("GET /api/health", s.apiHealth)
	mux.HandleFunc("POST /api/health/override", s.apiHealthOverride)
	mux.HandleFunc("GET /api/logs", s.apiLogs)
	mux.HandleFunc("GET /api/sync", s.apiSync)
	mux.HandleFunc("POST /api/sync/transfer", s.apiSyncTransfer)
	mux.HandleFunc("POST /api/sync/notify", s.apiSyncNotify)
	return mux
}

func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{
			"Page": name,
			"Role": s.Role,
			"Zone": r.URL.Query().Get("zone"),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := s.tpl.ExecuteTemplate(w, name+".html", data); err != nil {
			http.Error(w, err.Error(), 500)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

type zoneInfo struct {
	Name    string `json:"name"`
	Serial  uint32 `json:"serial"`
	Records int    `json:"records"`
}

func (s *Server) apiZones(w http.ResponseWriter, r *http.Request) {
	var out []zoneInfo
	for _, name := range s.Store.Zones() {
		z := s.Store.GetZone(name)
		out = append(out, zoneInfo{Name: name, Serial: z.Serial(), Records: len(z.Records)})
	}
	writeJSON(w, out)
}

func (s *Server) apiCreateZone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		NS   string `json:"ns"`
		Mbox string `json:"mbox"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Name == "" || req.NS == "" || req.Mbox == "" {
		writeErr(w, fmt.Errorf("name, ns, mbox 都是必填"))
		return
	}
	if err := s.Store.CreateZone(req.Name, req.NS, req.Mbox); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiDeleteZone(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteZone(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiRecords(w http.ResponseWriter, r *http.Request) {
	z := s.Store.GetZone(r.PathValue("name"))
	if z == nil {
		writeErr(w, fmt.Errorf("zone not found"))
		return
	}
	writeJSON(w, z.SortedRecords())
}

func (s *Server) apiAddRecord(w http.ResponseWriter, r *http.Request) {
	var rec zone.Record
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.Store.AddRecord(r.PathValue("name"), rec); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiUpdateRecord(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldName  string      `json:"old_name"`
		OldType  uint16      `json:"old_type"`
		OldRData string      `json:"old_rdata"`
		Record   zone.Record `json:"record"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.Store.UpdateRecord(r.PathValue("name"), req.OldName, req.OldType, req.OldRData, req.Record); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiDeleteRecord(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	t, ok := dnsmsg.StringToType(strings.ToUpper(q.Get("type")))
	if !ok {
		writeErr(w, fmt.Errorf("unknown type %q", q.Get("type")))
		return
	}
	if err := s.Store.DeleteRecord(r.PathValue("name"), q.Get("name"), t, q.Get("rdata")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	z := s.Store.GetZone(r.PathValue("name"))
	if z == nil {
		writeErr(w, fmt.Errorf("zone not found"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%szone", strings.TrimSuffix(z.Name, ".")))
	io.WriteString(w, zone.ExportZoneFile(z))
}

func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeErr(w, err)
		return
	}
	z, err := zone.ParseZoneFile(strings.NewReader(string(body)))
	if err != nil {
		writeErr(w, err)
		return
	}
	// importing replaces the whole zone; the zone name comes from the file
	s.Store.ReplaceZone(z, true)
	writeJSON(w, map[string]any{"status": "ok", "zone": z.Name, "records": len(z.Records)})
}

func (s *Server) apiHealth(w http.ResponseWriter, r *http.Request) {
	if s.Health == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, s.Health.Snapshot())
}

func (s *Server) apiHealthOverride(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RecKey string `json:"rec_key"`
		Up     *bool  `json:"up"` // null clears the override
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	if s.Health == nil {
		writeErr(w, fmt.Errorf("health checker not running"))
		return
	}
	s.Health.SetOverride(req.RecKey, req.Up)
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 200
	writeJSON(w, s.Log.Query(q.Get("qname"), q.Get("client"), limit))
}

type syncStatus struct {
	Role   string            `json:"role"`
	Zones  []zoneSyncInfo    `json:"zones"`
	Slaves []string          `json:"slaves,omitempty"`
	Slave  []xfr.SlaveStatus `json:"slave_status,omitempty"`
}

type zoneSyncInfo struct {
	Name    string `json:"name"`
	Serial  uint32 `json:"serial"`
	Journal int    `json:"journal"`
}

func (s *Server) apiSync(w http.ResponseWriter, r *http.Request) {
	st := syncStatus{Role: s.Role}
	for _, name := range s.Store.Zones() {
		z := s.Store.GetZone(name)
		st.Zones = append(st.Zones, zoneSyncInfo{
			Name: name, Serial: z.Serial(), Journal: len(s.Store.Journal(name)),
		})
	}
	if s.Master != nil {
		st.Slaves = s.Master.Slaves
	}
	if s.Slave != nil {
		st.Slave = s.Slave.Status()
	}
	writeJSON(w, st)
}

func (s *Server) apiSyncTransfer(w http.ResponseWriter, r *http.Request) {
	if s.Slave == nil {
		writeErr(w, fmt.Errorf("not a slave"))
		return
	}
	var req struct {
		Zone string `json:"zone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	go func() {
		s.Slave.Transfer(req.Zone) // async; result visible in /api/sync
	}()
	writeJSON(w, map[string]string{"status": "started"})
}

func (s *Server) apiSyncNotify(w http.ResponseWriter, r *http.Request) {
	if s.Master == nil {
		writeErr(w, fmt.Errorf("not a master"))
		return
	}
	var req struct {
		Zone string `json:"zone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	z := s.Store.GetZone(req.Zone)
	if z == nil {
		writeErr(w, fmt.Errorf("zone not found"))
		return
	}
	s.Master.NotifySlaves(z.Name, z.Serial())
	writeJSON(w, map[string]string{"status": "ok"})
}
