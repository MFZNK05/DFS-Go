// Package e2e_test contains end-to-end tests for the DFS-Go distributed file
// system. Tests spin up real in-process Server nodes on localhost TCP, using
// actual encryption, actual zstd compression, and actual on-disk storage.
package e2e_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	server "github.com/Faizan2005/DFS-Go/Server"
)

// TestMain configures the environment once for all E2E tests.
// We use TCP so ports are predictable and tests are fast.
func TestMain(m *testing.M) {
	os.Setenv("DFS_TRANSPORT", "tcp")
	// 32-byte key required by AES-256-GCM encryption service.
	os.Setenv("DFS_ENCRYPTION_KEY", "testkey-32bytes-padded-exactly!!")

	// Purge any stale artifacts left by previously crashed/killed test runs.
	// Without this, gopls indexes thousands of chunk files and OOMs VS Code.
	purgeStaleArtifacts()

	os.Exit(m.Run())
}

// purgeStaleArtifacts removes any leftover _network/ dirs and _metadata.json
// files that were not cleaned up by a prior test run (e.g. killed by SIGKILL).
func purgeStaleArtifacts() {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "127.0.0.1:") {
			if e.IsDir() && strings.HasSuffix(name, "_network") {
				os.RemoveAll(name)
			} else if !e.IsDir() && strings.HasSuffix(name, "_metadata.json") {
				os.Remove(name)
			}
		}
	}
}

// ----------------------------------------------------------------------------
// cluster — manages a set of in-process DFS nodes for a single test
// ----------------------------------------------------------------------------

type cluster struct {
	nodes []*server.Server
	addrs []string
	t     testing.TB
}

// newCluster starts n nodes with the given replication factor.
// All nodes are connected to each other (each bootstraps from all others).
// The cluster is torn down automatically via t.Cleanup.
func newCluster(t testing.TB, n, replFactor int) *cluster {
	t.Helper()

	addrs := make([]string, n)
	for i := range addrs {
		addrs[i] = freePort(t)
	}

	nodes := make([]*server.Server, n)
	for i := range nodes {
		// Each node bootstraps from all other nodes.
		peers := make([]string, 0, n-1)
		for j, a := range addrs {
			if j != i {
				peers = append(peers, a)
			}
		}
		nodes[i] = server.MakeServer(addrs[i], replFactor, peers...)
	}

	// Start all nodes in background goroutines.
	for _, nd := range nodes {
		go func(s *server.Server) { _ = s.Run() }(nd)
	}

	c := &cluster{nodes: nodes, addrs: addrs, t: t}

	// Wait until every node sees n-1 peers via its health endpoint.
	if n > 1 {
		waitFor(t, 10*time.Second, func() bool {
			for i, a := range addrs {
				pc := peerCountHTTP(t, healthAddr(a))
				_ = i
				if pc < n-1 {
					return false
				}
			}
			return true
		}, fmt.Sprintf("cluster of %d nodes did not form within timeout", n))
	}

	t.Cleanup(func() { c.teardown() })
	return c
}

func (c *cluster) teardown() {
	for _, nd := range c.nodes {
		if nd != nil {
			nd.GracefulShutdown()
		}
	}
	time.Sleep(300 * time.Millisecond)
	for _, a := range c.addrs {
		cleanNodeDirs(a)
	}
}

// addNode starts a new node bootstrapped to bootstrapAddr, adds it to the
// cluster, and waits for the cluster to see it.
func (c *cluster) addNode(bootstrapAddr string) *server.Server {
	c.t.Helper()
	addr := freePort(c.t)
	nd := server.MakeServer(addr, 3, bootstrapAddr)
	go func() { _ = nd.Run() }()
	c.nodes = append(c.nodes, nd)
	c.addrs = append(c.addrs, addr)

	waitFor(c.t, 8*time.Second, func() bool {
		return peerCountHTTP(c.t, healthAddr(addr)) >= 1
	}, "new node did not connect within timeout")

	return nd
}

// ----------------------------------------------------------------------------
// Test helpers
// ----------------------------------------------------------------------------

// waitFor polls condition every 100ms until it returns true or timeout.
func waitFor(t testing.TB, timeout time.Duration, condition func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("waitFor timeout: %s", msg)
}

// assertRoundtrip stores data on storeNode and retrieves it from getNode,
// asserting the bytes are identical.
func assertRoundtrip(t testing.TB, storeNode, getNode *server.Server, key string, data []byte) {
	t.Helper()
	if err := storeNode.StoreData(key, bytes.NewReader(data)); err != nil {
		t.Fatalf("StoreData(%q): %v", key, err)
	}
	// Give replication a moment to propagate when getNode != storeNode.
	if storeNode != getNode {
		time.Sleep(300 * time.Millisecond)
	}
	got := mustGetData(t, getNode, key)
	if !bytes.Equal(got, data) {
		t.Fatalf("roundtrip mismatch for %q: stored %d bytes, got %d bytes (sha256 want=%x got=%x)",
			key, len(data), len(got), sha256.Sum256(data), sha256.Sum256(got))
	}
}

// mustGetData retrieves data from node or fatals.
func mustGetData(t testing.TB, nd *server.Server, key string) []byte {
	t.Helper()
	r, err := nd.GetData(key)
	if err != nil {
		t.Fatalf("GetData(%q): %v", key, err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll(%q): %v", key, err)
	}
	return b
}

// sha256hex returns the hex SHA-256 of data.
func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}

// freePort allocates a free TCP port and returns ":PORT".
func freePort(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// healthAddr converts a P2P address to its health endpoint URL.
// MakeServer assigns health port = P2P port + 1000.
func healthAddr(p2pAddr string) string {
	parts := strings.Split(p2pAddr, ":")
	portStr := parts[len(parts)-1]
	port, _ := strconv.Atoi(portStr)
	return fmt.Sprintf("http://127.0.0.1:%d", port+1000)
}

// healthStatus fetches /health and returns the decoded JSON body.
func healthStatus(t testing.TB, addr string) map[string]any {
	t.Helper()
	resp, err := http.Get(addr + "/health")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil
	}
	return m
}

// peerCountHTTP fetches /health and returns the peer_count field (or -1 on error).
func peerCountHTTP(t testing.TB, addr string) int {
	m := healthStatus(t, addr)
	if m == nil {
		return -1
	}
	v, ok := m["peer_count"]
	if !ok {
		return -1
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	}
	return -1
}

// randomBytes returns n cryptographically random bytes.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// makePayload builds a byte slice of the requested type and size.
//
// Types:
//   - "text"   — repeated ASCII (highly compressible)
//   - "html"   — HTML markup (highly compressible)
//   - "csv"    — CSV rows (compressible)
//   - "jpeg"   — 0xFF 0xD8 0xFF header + random (should NOT compress)
//   - "png"    — 0x89 PNG header + random (should NOT compress)
//   - "zip"    — PK\x03\x04 header + random (should NOT compress)
//   - "gzip"   — \x1f\x8b header + random (should NOT compress)
//   - "binary" — pure random (should NOT compress)
func makePayload(typ string, size int) []byte {
	switch typ {
	case "text":
		unit := []byte("The quick brown fox jumps over the lazy dog. ")
		return repeatTo(unit, size)
	case "html":
		unit := []byte("<html><body><h1>Hello World</h1><p>Content goes here.</p></body></html>\n")
		return repeatTo(unit, size)
	case "csv":
		unit := []byte("id,name,email,score,timestamp\n1,Alice,alice@example.com,99,2024-01-01\n")
		return repeatTo(unit, size)
	case "jpeg":
		hdr := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46}
		return append(hdr, randomBytes(size-len(hdr))...)
	case "png":
		hdr := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
		return append(hdr, randomBytes(size-len(hdr))...)
	case "zip":
		hdr := []byte{0x50, 0x4B, 0x03, 0x04}
		return append(hdr, randomBytes(size-len(hdr))...)
	case "gzip":
		hdr := []byte{0x1F, 0x8B}
		return append(hdr, randomBytes(size-len(hdr))...)
	case "binary":
		return randomBytes(size)
	default:
		panic("makePayload: unknown type " + typ)
	}
}

func repeatTo(unit []byte, size int) []byte {
	out := make([]byte, 0, size)
	for len(out) < size {
		rem := size - len(out)
		if rem >= len(unit) {
			out = append(out, unit...)
		} else {
			out = append(out, unit[:rem]...)
		}
	}
	return out
}

// readerOf wraps a byte slice in an io.Reader.
func readerOf(b []byte) io.Reader {
	return bytes.NewReader(b)
}

// cleanNodeDirs removes the on-disk storage directories left by a node.
func cleanNodeDirs(addr string) {
	os.RemoveAll(addr + "_network")
	os.Remove(addr + "_metadata.json")
}
