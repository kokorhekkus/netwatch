// Package server exposes the dashboard and its JSON API on loopback.
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/kokorhekkus/netwatch/internal/app"
	"github.com/kokorhekkus/netwatch/internal/store"
	"github.com/kokorhekkus/netwatch/internal/web"
)

const DefaultAddr = "127.0.0.1:7717"

type Server struct {
	db  *store.DB
	log *slog.Logger
}

func New(db *store.DB, log *slog.Logger) *Server {
	return &Server{db: db, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	assets, err := fs.Sub(web.Assets, "assets")
	if err != nil {
		panic(err) // embedded at build time; cannot fail at runtime
	}
	mux.Handle("/", http.FileServer(http.FS(assets)))

	mux.HandleFunc("/api/summary", s.handleSummary)
	mux.HandleFunc("/api/series", s.handleSeries)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/layers", s.handleLayers)

	return mux
}

type layersResponse struct {
	Throughput []store.ThroughputRow `json:"throughput"`
	DNS        []store.DNSSummary    `json:"dns"`
	HTTP       []store.HTTPSummary   `json:"http"`
	WiFi       []store.WiFiPoint     `json:"wifi"`
	WiFiNow    *store.WiFiPoint      `json:"wifiNow"`
}

// handleLayers returns everything needed to attribute a slow connection to a
// specific layer: capacity and bufferbloat, DNS timing per resolver, and the
// phases of an HTTPS request.
func (s *Server) handleLayers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	from, to, points := timeRange(r)

	tp, err := s.db.QueryThroughput(ctx, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dns, err := s.db.QueryDNSSummary(ctx, from)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpSum, err := s.db.QueryHTTPSummary(ctx, from)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	radio, err := s.db.QueryWiFi(ctx, from, to, points)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	latest, ok, err := s.db.LatestWiFi(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if tp == nil {
		tp = []store.ThroughputRow{}
	}
	if dns == nil {
		dns = []store.DNSSummary{}
	}
	if httpSum == nil {
		httpSum = []store.HTTPSummary{}
	}
	if radio == nil {
		radio = []store.WiFiPoint{}
	}

	resp := layersResponse{Throughput: tp, DNS: dns, HTTP: httpSum, WiFi: radio}
	if ok {
		resp.WiFiNow = &latest
	}
	writeJSON(w, resp)
}

// ListenAndServe binds the dashboard.
//
// The address is loopback-only and explicit. Binding the wildcard would make
// this an incoming connection as far as the Application Firewall is concerned,
// which prompts the user on every rebuild of an ad-hoc-signed binary.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	s.log.Info("dashboard listening", "url", "http://"+addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// timeRange reads from/to/points, defaulting to the last six hours.
func timeRange(r *http.Request) (from, to time.Time, points int) {
	now := time.Now()
	to = now
	from = now.Add(-6 * time.Hour)

	q := r.URL.Query()
	if v := q.Get("from"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			from = time.Unix(n, 0)
		}
	}
	if v := q.Get("to"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			to = time.Unix(n, 0)
		}
	}
	points = 1000
	if v := q.Get("points"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			points = n
		}
	}
	return
}

type summaryResponse struct {
	Network     string             `json:"network"`
	Fingerprint string             `json:"fingerprint"`
	Kind        string             `json:"kind"`
	Window      string             `json:"window"`
	Targets     []app.TargetStatus `json:"targets"`
	Generated   int64              `json:"generated"`
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	window := time.Hour
	if v := r.URL.Query().Get("window"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			window = d
		}
	}

	sts, err := app.Status(ctx, s.db, window)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	label, fp, kind, err := s.db.CurrentNetwork(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, summaryResponse{
		Network:     label,
		Fingerprint: fp,
		Kind:        kind,
		Window:      window.String(),
		Targets:     sts,
		Generated:   time.Now().Unix(),
	})
}

type seriesResponse struct {
	From   int64          `json:"from"`
	To     int64          `json:"to"`
	Grain  int            `json:"grain"`
	Series []store.Series `json:"series"`
	Gaps   []store.Gap    `json:"gaps"`
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	from, to, points := timeRange(r)

	series, err := s.db.QuerySeries(ctx, from, to, points, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	gaps, err := s.db.QueryGaps(ctx, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, seriesResponse{
		From:   from.Unix(),
		To:     to.Unix(),
		Grain:  store.PickGrain(to.Unix()-from.Unix(), points),
		Series: series,
		Gaps:   gaps,
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	from, to, _ := timeRange(r)
	events, err := s.db.QueryEvents(r.Context(), from, to, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []store.Event{}
	}
	writeJSON(w, events)
}
