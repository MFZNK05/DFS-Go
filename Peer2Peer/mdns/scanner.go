package mdns

import (
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	hmdns "github.com/hashicorp/mdns"
)

// DiscoveredPeer holds the metadata of a node found via mDNS scan.
type DiscoveredPeer struct {
	Alias       string `json:"alias"`
	Fingerprint string `json:"fingerprint"`
	Addr        string `json:"addr"` // host:port
}

// Scan performs an mDNS query on the LAN for Hermod nodes.
// Returns discovered peers within the given timeout.
func Scan(timeout time.Duration) ([]DiscoveredPeer, error) {
	ch := make(chan *hmdns.ServiceEntry, 16)

	var peers []DiscoveredPeer
	done := make(chan struct{})
	go func() {
		for entry := range ch {
			peer := entryToPeer(entry)
			if peer != nil {
				peers = append(peers, *peer)
			}
		}
		close(done)
	}()

	params := hmdns.DefaultParams(ServiceTag)
	params.Entries = ch
	params.Timeout = timeout
	params.DisableIPv6 = true
	params.Logger = log.New(io.Discard, "", 0)

	err := hmdns.Query(params)
	close(ch)
	<-done

	if err != nil {
		return nil, fmt.Errorf("mdns scan: %w", err)
	}
	return peers, nil
}

// entryToPeer converts an mDNS service entry to a DiscoveredPeer.
func entryToPeer(e *hmdns.ServiceEntry) *DiscoveredPeer {
	if e == nil {
		return nil
	}

	var alias, fp, port string
	for _, txt := range e.InfoFields {
		switch {
		case strings.HasPrefix(txt, "alias="):
			alias = strings.TrimPrefix(txt, "alias=")
		case strings.HasPrefix(txt, "fp="):
			fp = strings.TrimPrefix(txt, "fp=")
		case strings.HasPrefix(txt, "port="):
			port = strings.TrimPrefix(txt, "port=")
		}
	}

	// Determine host IP — prefer IPv4.
	host := e.AddrV4.String()
	if e.AddrV4 == nil {
		if e.AddrV6 != nil {
			host = e.AddrV6.String()
		} else {
			return nil
		}
	}

	// Use port from TXT record (authoritative) or fallback to service port.
	if port == "" {
		port = fmt.Sprintf("%d", e.Port)
	}

	return &DiscoveredPeer{
		Alias:       alias,
		Fingerprint: fp,
		Addr:        fmt.Sprintf("%s:%s", host, port),
	}
}
