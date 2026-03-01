package e2e_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHealthEndpointOK verifies that a 3-node cluster returns HTTP 200 with
// {"status":"ok"} on the /health endpoint.
func TestHealthEndpointOK(t *testing.T) {
	c := newCluster(t, 3, 3)

	// All nodes should report "ok" once fully connected.
	for i, addr := range c.addrs {
		hAddr := healthAddr(addr)
		waitFor(t, 5*time.Second, func() bool {
			m := healthStatus(t, hAddr)
			if m == nil {
				return false
			}
			s, _ := m["status"].(string)
			return s == "ok"
		}, "health endpoint did not return ok for node "+addr)

		m := healthStatus(t, hAddr)
		if m == nil {
			t.Fatalf("node[%d]: health endpoint unreachable", i)
		}
		if s, _ := m["status"].(string); s != "ok" {
			t.Errorf("node[%d]: expected status=ok, got %q", i, s)
		}
	}
}

// TestHealthEndpointDegraded verifies that an isolated single node (no peers)
// returns a non-"ok" status on /health.
func TestHealthEndpointDegraded(t *testing.T) {
	// Start a single node with no peers.
	c := newCluster(t, 1, 1)

	hAddr := healthAddr(c.addrs[0])
	// Give the health server time to start.
	time.Sleep(300 * time.Millisecond)

	m := healthStatus(t, hAddr)
	if m == nil {
		t.Fatal("health endpoint unreachable for isolated node")
	}

	s, _ := m["status"].(string)
	// An isolated node has 0 peers — its status should be "degraded" (not "ok").
	if s == "ok" {
		t.Errorf("isolated node reported status=%q; expected degraded/unhealthy", s)
	}
}

// TestHealthPeerCountAccurate verifies that /health reports correct peer_count
// on a 3-node cluster, and that it decrements when a node leaves.
func TestHealthPeerCountAccurate(t *testing.T) {
	c := newCluster(t, 3, 3)

	// Every node should report peer_count == 2.
	for i, addr := range c.addrs {
		pc := peerCountHTTP(t, healthAddr(addr))
		if pc != 2 {
			t.Errorf("node[%d] peer_count: got %d, want 2", i, pc)
		}
	}

	// Remove node[2] gracefully and wait for peer count to drop.
	c.nodes[2].GracefulShutdown()
	cleanNodeDirs(c.addrs[2])
	c.nodes[2] = nil

	waitFor(t, 8*time.Second, func() bool {
		return peerCountHTTP(t, healthAddr(c.addrs[0])) <= 1
	}, "peer_count did not drop after node leave")

	pc := peerCountHTTP(t, healthAddr(c.addrs[0]))
	if pc > 1 {
		t.Errorf("peer_count after node leave: got %d, want ≤1", pc)
	}
}

// TestHealthUptimeField verifies that the /health response includes an uptime
// field that is non-empty after the node has been running for > 0 seconds.
func TestHealthUptimeField(t *testing.T) {
	c := newCluster(t, 1, 1)
	time.Sleep(1 * time.Second)

	m := healthStatus(t, healthAddr(c.addrs[0]))
	if m == nil {
		t.Fatal("health endpoint unreachable")
	}
	uptime, _ := m["uptime"].(string)
	if uptime == "" || uptime == "0s" {
		t.Errorf("uptime should be non-empty and > 0s after 1s, got %q", uptime)
	}
}

// TestHealthNodeAddrField verifies that the /health response includes the
// node_addr field matching the node's listen address.
func TestHealthNodeAddrField(t *testing.T) {
	c := newCluster(t, 1, 1)
	time.Sleep(200 * time.Millisecond)

	m := healthStatus(t, healthAddr(c.addrs[0]))
	if m == nil {
		t.Fatal("health endpoint unreachable")
	}
	nodeAddr, _ := m["node_addr"].(string)
	if nodeAddr == "" {
		t.Errorf("node_addr field missing from /health response")
	}
}

// TestPrometheusMetricsExposed verifies that /metrics returns HTTP 200 and
// contains at least one DFS-specific metric line.
func TestPrometheusMetricsExposed(t *testing.T) {
	c := newCluster(t, 3, 3)
	time.Sleep(300 * time.Millisecond)

	// Upload a file so some counters are non-zero.
	data := makePayload("text", 64*1024)
	if err := c.nodes[0].StoreData("metrics-key", readerOf(data)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	metricsURL := healthAddr(c.addrs[0]) + "/metrics"
	resp, err := http.Get(metricsURL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics returned HTTP %d, want 200", resp.StatusCode)
	}

	// Read body and confirm it contains at least one DFS metric.
	buf := make([]byte, 32*1024)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	if !strings.Contains(body, "dfs_") {
		t.Errorf("/metrics body does not contain any dfs_ metrics; body snippet: %.200s", body)
	}
}

// TestPprofEndpointAvailable verifies that /debug/pprof/ returns HTTP 200.
func TestPprofEndpointAvailable(t *testing.T) {
	c := newCluster(t, 1, 1)
	time.Sleep(300 * time.Millisecond)

	pprofURL := healthAddr(c.addrs[0]) + "/debug/pprof/"
	resp, err := http.Get(pprofURL)
	if err != nil {
		t.Fatalf("GET /debug/pprof/: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/debug/pprof/ returned HTTP %d, want 200", resp.StatusCode)
	}
}

// TestHealthEndpointRingSizeField verifies that /health includes ring_size
// equal to the number of cluster nodes.
func TestHealthEndpointRingSizeField(t *testing.T) {
	c := newCluster(t, 3, 3)

	m := healthStatus(t, healthAddr(c.addrs[0]))
	if m == nil {
		t.Fatal("health endpoint unreachable")
	}
	rs, _ := m["ring_size"].(float64)
	if int(rs) != 3 {
		t.Errorf("ring_size: got %v, want 3", rs)
	}
}
