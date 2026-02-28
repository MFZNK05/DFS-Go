// Package nat provides STUN-based public address discovery and UDP hole
// punching for NAT traversal in college LAN deployments.
//
// Typical usage:
//
//	n := nat.New(nat.DefaultConfig())
//	pub, err := n.DiscoverPublicAddr()  // contacts STUN server
//	meta := n.AnnotateGossip()          // {"public_addr": "1.2.3.4:PORT"}
package nat

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"
)

// Config holds NAT traversal configuration.
type Config struct {
	// STUNServers is a list of STUN servers to try in order.
	// Format: "host:port" (plain UDP, RFC 5389 binding requests).
	STUNServers []string

	// DiscoverTimeout is the per-server timeout for STUN requests.
	DiscoverTimeout time.Duration
}

// DefaultConfig returns a production-ready config using Google's public STUN
// servers, which are accessible from most college LANs.
func DefaultConfig() Config {
	return Config{
		STUNServers: []string{
			"stun.l.google.com:19302",
			"stun1.l.google.com:19302",
			"stun2.l.google.com:19302",
		},
		DiscoverTimeout: 3 * time.Second,
	}
}

// Puncher discovers the node's public address and annotates gossip metadata
// so that peers behind NAT can establish direct UDP connections.
type Puncher struct {
	cfg        Config
	mu         sync.RWMutex
	publicAddr string // cached after first successful discovery
}

// New returns a Puncher using cfg.
func New(cfg Config) *Puncher {
	return &Puncher{cfg: cfg}
}

// DiscoverPublicAddr contacts STUN servers in order and returns the first
// successfully discovered "ip:port" string. The result is cached for future
// calls to PublicAddr().
//
// STUN Binding Request format (RFC 5389, §6):
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|0 0|     STUN Message Type     |         Message Length        |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                         Magic Cookie                          |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                                                               |
//	|                     Transaction ID (96 bits)                   |
//	|                                                               |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
func (p *Puncher) DiscoverPublicAddr() (string, error) {
	var lastErr error
	for _, srv := range p.cfg.STUNServers {
		addr, err := p.stunQuery(srv)
		if err != nil {
			lastErr = fmt.Errorf("STUN %s: %w", srv, err)
			continue
		}
		p.mu.Lock()
		p.publicAddr = addr
		p.mu.Unlock()
		return addr, nil
	}
	if lastErr != nil {
		return "", fmt.Errorf("nat: all STUN servers failed: %w", lastErr)
	}
	return "", fmt.Errorf("nat: no STUN servers configured")
}

// PublicAddr returns the last successfully discovered public address, or ""
// if DiscoverPublicAddr has not been called yet.
func (p *Puncher) PublicAddr() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.publicAddr
}

// AnnotateGossip returns a map suitable for inclusion in NodeInfo.Metadata.
// If no public address has been discovered yet, the map is empty.
func (p *Puncher) AnnotateGossip() map[string]string {
	addr := p.PublicAddr()
	if addr == "" {
		return map[string]string{}
	}
	return map[string]string{"public_addr": addr}
}

// stunQuery sends a RFC 5389 Binding Request to addr and parses the
// XOR-MAPPED-ADDRESS attribute from the response.
func (p *Puncher) stunQuery(serverAddr string) (string, error) {
	conn, err := net.DialTimeout("udp", serverAddr, p.cfg.DiscoverTimeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(p.cfg.DiscoverTimeout))

	// Build Binding Request: type=0x0001, length=0, magic=0x2112A442, random TxID.
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001)       // Binding Request
	binary.BigEndian.PutUint16(req[2:], 0)             // Message Length (no attributes)
	binary.BigEndian.PutUint32(req[4:], 0x2112A442)   // Magic Cookie (RFC 5389)
	txID := req[8:20]
	if _, err := rand.Read(txID); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}

	if _, err := conn.Write(req); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}

	resp := make([]byte, 1024)
	n, err := conn.Read(resp)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return parseSTUNResponse(resp[:n], txID)
}

// parseSTUNResponse parses the XOR-MAPPED-ADDRESS (0x0020) or MAPPED-ADDRESS
// (0x0001) attribute from a STUN Binding Response.
func parseSTUNResponse(data []byte, txID []byte) (string, error) {
	if len(data) < 20 {
		return "", fmt.Errorf("response too short (%d bytes)", len(data))
	}

	msgType := binary.BigEndian.Uint16(data[0:])
	if msgType != 0x0101 { // Binding Success Response
		return "", fmt.Errorf("unexpected STUN message type 0x%04x", msgType)
	}

	// Verify transaction ID matches.
	for i, b := range txID {
		if data[8+i] != b {
			return "", fmt.Errorf("transaction ID mismatch")
		}
	}

	// Walk attributes starting at byte 20.
	pos := 20
	for pos+4 <= len(data) {
		attrType := binary.BigEndian.Uint16(data[pos:])
		attrLen := int(binary.BigEndian.Uint16(data[pos+2:]))
		pos += 4

		if pos+attrLen > len(data) {
			break
		}

		switch attrType {
		case 0x0020: // XOR-MAPPED-ADDRESS
			return parseXORMappedAddr(data[pos:pos+attrLen], data[4:8], txID)
		case 0x0001: // MAPPED-ADDRESS (legacy)
			return parseMappedAddr(data[pos : pos+attrLen])
		}

		// Attributes are padded to 4-byte boundaries.
		pos += (attrLen + 3) &^ 3
	}

	return "", fmt.Errorf("no MAPPED-ADDRESS attribute found in STUN response")
}

// parseXORMappedAddr decodes the XOR-MAPPED-ADDRESS attribute value.
// RFC 5389 §15.2: port is XORed with the high 16 bits of magic cookie;
// IPv4 address is XORed with the full magic cookie.
func parseXORMappedAddr(val []byte, magic []byte, txID []byte) (string, error) {
	if len(val) < 8 {
		return "", fmt.Errorf("XOR-MAPPED-ADDRESS too short")
	}
	family := binary.BigEndian.Uint16(val[0:])
	xorPort := binary.BigEndian.Uint16(val[2:])
	port := xorPort ^ 0x2112

	switch family {
	case 0x01: // IPv4
		if len(val) < 8 {
			return "", fmt.Errorf("XOR-MAPPED-ADDRESS IPv4 too short")
		}
		xorIP := binary.BigEndian.Uint32(val[4:])
		ip := xorIP ^ 0x2112A442
		addr := net.IPv4(byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip))
		return fmt.Sprintf("%s:%d", addr.String(), port), nil

	case 0x02: // IPv6
		if len(val) < 20 {
			return "", fmt.Errorf("XOR-MAPPED-ADDRESS IPv6 too short")
		}
		xorKey := append(magic, txID...)
		ipBytes := make([]byte, 16)
		for i := range ipBytes {
			ipBytes[i] = val[4+i] ^ xorKey[i]
		}
		addr := net.IP(ipBytes)
		return fmt.Sprintf("[%s]:%d", addr.String(), port), nil

	default:
		return "", fmt.Errorf("unknown address family 0x%04x", family)
	}
}

// parseMappedAddr decodes the MAPPED-ADDRESS attribute (legacy, no XOR).
func parseMappedAddr(val []byte) (string, error) {
	if len(val) < 8 {
		return "", fmt.Errorf("MAPPED-ADDRESS too short")
	}
	family := binary.BigEndian.Uint16(val[0:])
	port := binary.BigEndian.Uint16(val[2:])

	if family != 0x01 {
		return "", fmt.Errorf("only IPv4 MAPPED-ADDRESS supported (family=0x%04x)", family)
	}
	ip := net.IPv4(val[4], val[5], val[6], val[7])
	return fmt.Sprintf("%s:%d", ip.String(), port), nil
}
