// Package mdns provides mDNS-based LAN auto-discovery for Hermond nodes.
// When a node starts, it advertises itself via mDNS so other nodes on the
// same WiFi/LAN can discover it without typing IP addresses.
package mdns

import (
	"fmt"
	"net"
	"strconv"

	hmdns "github.com/hashicorp/mdns"
)

const ServiceTag = "_hermond._udp"

// Advertiser broadcasts this node's presence on the LAN via mDNS.
type Advertiser struct {
	server *hmdns.Server
}

// NewAdvertiser creates and starts an mDNS service advertisement.
// host is the routable IP (use resolveOutboundIP, NOT 0.0.0.0).
// port is the QUIC listen port. alias and fingerprint are carried in TXT records.
func NewAdvertiser(host, alias, fingerprint string, port int) (*Advertiser, error) {
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("mdns: invalid host IP %q", host)
	}

	txt := []string{
		"alias=" + alias,
		"fp=" + fingerprint,
		"port=" + strconv.Itoa(port),
	}

	svc, err := hmdns.NewMDNSService(
		alias,     // instance name
		ServiceTag, // service type
		"",        // domain (default .local)
		"",        // host name (auto)
		port,      // port
		[]net.IP{ip}, // IPs to advertise
		txt,       // TXT records
	)
	if err != nil {
		return nil, fmt.Errorf("mdns: new service: %w", err)
	}

	server, err := hmdns.NewServer(&hmdns.Config{Zone: svc})
	if err != nil {
		return nil, fmt.Errorf("mdns: start server: %w", err)
	}

	return &Advertiser{server: server}, nil
}

// Stop shuts down the mDNS advertiser.
func (a *Advertiser) Stop() {
	if a != nil && a.server != nil {
		a.server.Shutdown()
	}
}
