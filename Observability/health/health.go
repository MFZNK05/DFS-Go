// Package health provides an HTTP health-check server for DFS-Go nodes.
//
// Three endpoints are served:
//
//	GET /health   — JSON node status (200 OK or 503 Service Unavailable)
//	GET /metrics  — Prometheus text exposition (via promhttp)
//	GET /debug/pprof/... — Go runtime profiling (via net/http/pprof)
//
// Usage:
//
//	srv := health.New(":4000", func() health.Status {
//	    return health.Status{Status: "ok", NodeAddr: "127.0.0.1:3000", PeerCount: 3}
//	}, prometheus.DefaultGatherer)
//	if err := srv.Start(); err != nil { log.Fatal(err) }
//	// ...
//	srv.Stop(ctx)
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	// blank import registers pprof handlers on DefaultServeMux;
	// we use our own mux so we register them manually below.
	_ "net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Status is the payload returned by /health.
type Status struct {
	Status    string `json:"status"`              // "ok" | "degraded"
	NodeAddr  string `json:"node_addr"`           // this node's P2P listen address
	PeerCount int    `json:"peer_count"`
	RingSize  int    `json:"ring_size"`
	Uptime    string `json:"uptime"`              // human-readable, e.g. "3h12m"
	StartedAt string `json:"started_at,omitempty"` // RFC3339
}

// Server serves /health, /metrics, and /debug/pprof/ endpoints.
type Server struct {
	httpServer *http.Server
	getStatus  func() Status
	gatherer   prometheus.Gatherer
}

// New creates a HealthServer that listens on listenAddr.
// getStatus is called on every /health request.
// gatherer is used to serve /metrics; pass prometheus.DefaultGatherer if using
// the default registry, or a custom *prometheus.Registry otherwise.
func New(listenAddr string, getStatus func() Status, gatherer prometheus.Gatherer) *Server {
	if gatherer == nil {
		gatherer = prometheus.DefaultGatherer
	}

	hs := &Server{
		getStatus: getStatus,
		gatherer:  gatherer,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", hs.serveHealth)
	mux.Handle("/metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))

	// Register pprof handlers explicitly (we don't use DefaultServeMux).
	mux.HandleFunc("/debug/pprof/", http.DefaultServeMux.ServeHTTP)

	hs.httpServer = &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	return hs
}

// Start begins listening and serving in a background goroutine.
// Returns an error if the port is already in use.
func (hs *Server) Start() error {
	ln, err := net.Listen("tcp", hs.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("health server listen %s: %w", hs.httpServer.Addr, err)
	}
	go func() {
		if err := hs.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			// Non-fatal: log would come from the server layer, not here.
			_ = err
		}
	}()
	return nil
}

// Stop gracefully shuts down the HTTP server within the given deadline.
func (hs *Server) Stop(ctx context.Context) error {
	return hs.httpServer.Shutdown(ctx)
}

// Addr returns the listen address (useful for tests that pick ":0").
func (hs *Server) Addr() string {
	return hs.httpServer.Addr
}

func (hs *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	st := hs.getStatus()
	w.Header().Set("Content-Type", "application/json")
	if st.Status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(st)
}
