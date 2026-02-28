package health_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Observability/health"
	"github.com/prometheus/client_golang/prometheus"
)

// freePort finds an available TCP port.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// newServer starts a health server on a random port, returning the server and its base URL.
func newServer(t *testing.T, getStatus func() health.Status) (*health.Server, string) {
	t.Helper()
	addr := freePort(t)
	reg := prometheus.NewRegistry()
	srv := health.New(addr, getStatus, reg)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Stop(ctx)
	})
	// Small pause for the listener to be ready.
	time.Sleep(10 * time.Millisecond)
	return srv, fmt.Sprintf("http://%s", addr)
}

// ---------------------------------------------------------------------------
// /health endpoint
// ---------------------------------------------------------------------------

func TestHealthOkReturns200(t *testing.T) {
	_, base := newServer(t, func() health.Status {
		return health.Status{Status: "ok", NodeAddr: "127.0.0.1:3000", PeerCount: 2}
	})

	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", resp.StatusCode)
	}
}

func TestHealthDegradedReturns503(t *testing.T) {
	_, base := newServer(t, func() health.Status {
		return health.Status{Status: "degraded", NodeAddr: "127.0.0.1:3000"}
	})

	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

func TestHealthResponseIsJSON(t *testing.T) {
	_, base := newServer(t, func() health.Status {
		return health.Status{Status: "ok", NodeAddr: "127.0.0.1:3000", PeerCount: 3, RingSize: 3}
	})

	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var st health.Status
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("response is not valid JSON: %s", body)
	}
	if st.Status != "ok" {
		t.Errorf("expected status=ok, got %q", st.Status)
	}
	if st.NodeAddr != "127.0.0.1:3000" {
		t.Errorf("expected node_addr=127.0.0.1:3000, got %q", st.NodeAddr)
	}
	if st.PeerCount != 3 {
		t.Errorf("expected peer_count=3, got %d", st.PeerCount)
	}
}

func TestHealthContentTypeJSON(t *testing.T) {
	_, base := newServer(t, func() health.Status {
		return health.Status{Status: "ok"}
	})
	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

// ---------------------------------------------------------------------------
// /metrics endpoint
// ---------------------------------------------------------------------------

func TestMetricsEndpointReachable(t *testing.T) {
	_, base := newServer(t, func() health.Status { return health.Status{Status: "ok"} })

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from /metrics, got %d", resp.StatusCode)
	}
}

func TestMetricsEndpointReturnsPrometheusText(t *testing.T) {
	// Register a test counter in a fresh registry so we get a non-empty body.
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_counter_total", Help: "test"})
	reg.MustRegister(c)
	c.Inc()

	addr := freePort(t)
	srv := health.New(addr, func() health.Status { return health.Status{Status: "ok"} }, reg)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Stop(ctx)
	})
	time.Sleep(10 * time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://%s/metrics", addr))
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "test_counter_total") {
		t.Errorf("expected test_counter_total in metrics body, got: %s", body)
	}
}

// ---------------------------------------------------------------------------
// /debug/pprof/
// ---------------------------------------------------------------------------

func TestPprofEndpointReachable(t *testing.T) {
	_, base := newServer(t, func() health.Status { return health.Status{Status: "ok"} })

	resp, err := http.Get(base + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET /debug/pprof/: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from /debug/pprof/, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Stop is idempotent / graceful
// ---------------------------------------------------------------------------

func TestStopGraceful(t *testing.T) {
	addr := freePort(t)
	reg := prometheus.NewRegistry()
	srv := health.New(addr, func() health.Status { return health.Status{Status: "ok"} }, reg)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
		t.Errorf("Stop returned error: %v", err)
	}
	// Second stop must not panic or error.
	_ = srv.Stop(ctx)
}

// ---------------------------------------------------------------------------
// getStatus called on every request
// ---------------------------------------------------------------------------

func TestGetStatusCalledPerRequest(t *testing.T) {
	calls := 0
	_, base := newServer(t, func() health.Status {
		calls++
		return health.Status{Status: "ok"}
	})

	for i := 0; i < 3; i++ {
		resp, err := http.Get(base + "/health")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
	}

	if calls != 3 {
		t.Errorf("expected getStatus called 3 times, got %d", calls)
	}
}
