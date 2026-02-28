package nat_test

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Peer2Peer/nat"
)

// startMockSTUN launches a fake STUN server that responds with a hard-coded
// XOR-MAPPED-ADDRESS value for any Binding Request.
// Returns the server address ("host:port") and a stop function.
func startMockSTUN(t *testing.T, reportedIP string, reportedPort uint16) (string, func()) {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startMockSTUN: %v", err)
	}

	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return // stopped
			}
			if n < 20 {
				continue
			}

			// Copy transaction ID from request.
			txID := make([]byte, 12)
			copy(txID, buf[8:20])

			// Build Binding Success Response (type=0x0101).
			resp := buildXORMappedResponse(txID, reportedIP, reportedPort)
			conn.WriteTo(resp, from)
		}
	}()

	addr := conn.(*net.UDPConn).LocalAddr().String()
	return addr, func() { conn.Close() }
}

// buildXORMappedResponse creates a minimal STUN Binding Success Response
// containing a single XOR-MAPPED-ADDRESS attribute for the given IPv4/port.
func buildXORMappedResponse(txID []byte, ip string, port uint16) []byte {
	const magic uint32 = 0x2112A442

	// XOR-MAPPED-ADDRESS value: 8 bytes for IPv4
	// [2B family(0x0001)][2B xor-port][4B xor-ip]
	xorPort := port ^ 0x2112
	ipBytes := net.ParseIP(ip).To4()
	xorIP := binary.BigEndian.Uint32(ipBytes) ^ magic

	attrVal := make([]byte, 8)
	binary.BigEndian.PutUint16(attrVal[0:], 0x0001) // IPv4
	binary.BigEndian.PutUint16(attrVal[2:], xorPort)
	binary.BigEndian.PutUint32(attrVal[4:], xorIP)

	// Attribute header: type=0x0020, length=8
	attr := make([]byte, 4+8)
	binary.BigEndian.PutUint16(attr[0:], 0x0020)
	binary.BigEndian.PutUint16(attr[2:], 8)
	copy(attr[4:], attrVal)

	// STUN header: 20 bytes
	msgLen := uint16(len(attr))
	header := make([]byte, 20)
	binary.BigEndian.PutUint16(header[0:], 0x0101) // Binding Success
	binary.BigEndian.PutUint16(header[2:], msgLen)
	binary.BigEndian.PutUint32(header[4:], magic)
	copy(header[8:], txID)

	return append(header, attr...)
}

// ---------------------------------------------------------------------------

func TestDiscoverPublicAddr(t *testing.T) {
	srvAddr, stop := startMockSTUN(t, "203.0.113.42", 54321)
	defer stop()

	p := nat.New(nat.Config{
		STUNServers:     []string{srvAddr},
		DiscoverTimeout: 2 * time.Second,
	})

	addr, err := p.DiscoverPublicAddr()
	if err != nil {
		t.Fatalf("DiscoverPublicAddr: %v", err)
	}
	if !strings.HasPrefix(addr, "203.0.113.42:") {
		t.Errorf("unexpected addr %q (want 203.0.113.42:...)", addr)
	}
	if !strings.HasSuffix(addr, ":54321") {
		t.Errorf("unexpected port in %q (want :54321)", addr)
	}
}

func TestDiscoverPublicAddrCachesResult(t *testing.T) {
	srvAddr, stop := startMockSTUN(t, "10.0.0.1", 9999)
	defer stop()

	p := nat.New(nat.Config{
		STUNServers:     []string{srvAddr},
		DiscoverTimeout: 2 * time.Second,
	})

	addr1, err := p.DiscoverPublicAddr()
	if err != nil {
		t.Fatalf("first discover: %v", err)
	}

	// Stop the server; second call must use cache.
	stop()

	addr2 := p.PublicAddr()
	if addr1 != addr2 {
		t.Errorf("cached addr %q != first addr %q", addr2, addr1)
	}
}

func TestPublicAddrBeforeDiscover(t *testing.T) {
	p := nat.New(nat.DefaultConfig())
	if got := p.PublicAddr(); got != "" {
		t.Errorf("expected empty before discover, got %q", got)
	}
}

func TestAnnotateGossipEmpty(t *testing.T) {
	p := nat.New(nat.DefaultConfig())
	meta := p.AnnotateGossip()
	if _, ok := meta["public_addr"]; ok {
		t.Error("expected no public_addr key before discovery")
	}
}

func TestAnnotateGossipPopulated(t *testing.T) {
	srvAddr, stop := startMockSTUN(t, "198.51.100.7", 12345)
	defer stop()

	p := nat.New(nat.Config{
		STUNServers:     []string{srvAddr},
		DiscoverTimeout: 2 * time.Second,
	})
	if _, err := p.DiscoverPublicAddr(); err != nil {
		t.Fatalf("discover: %v", err)
	}

	meta := p.AnnotateGossip()
	v, ok := meta["public_addr"]
	if !ok {
		t.Fatal("expected public_addr key in gossip metadata")
	}
	if !strings.Contains(v, "198.51.100.7") {
		t.Errorf("unexpected public_addr %q", v)
	}
}

func TestAllSTUNServersFail(t *testing.T) {
	p := nat.New(nat.Config{
		STUNServers:     []string{"127.0.0.1:1", "127.0.0.1:2"}, // refuse
		DiscoverTimeout: 200 * time.Millisecond,
	})
	_, err := p.DiscoverPublicAddr()
	if err == nil {
		t.Fatal("expected error when all STUN servers fail")
	}
}

func TestFallbackToSecondSTUNServer(t *testing.T) {
	// First server will never respond (port 1 is typically refused).
	goodAddr, stop := startMockSTUN(t, "192.0.2.55", 7777)
	defer stop()

	p := nat.New(nat.Config{
		STUNServers:     []string{"127.0.0.1:1", goodAddr},
		DiscoverTimeout: 500 * time.Millisecond,
	})
	addr, err := p.DiscoverPublicAddr()
	if err != nil {
		t.Fatalf("expected fallback to succeed: %v", err)
	}
	if !strings.Contains(addr, "192.0.2.55") {
		t.Errorf("unexpected fallback addr %q", addr)
	}
}
