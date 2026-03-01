// Package factory creates the correct peer2peer.Transport based on the
// DFS_TRANSPORT environment variable (default: "quic").
//
// Usage:
//
//	tr, err := factory.New("quic", ":3000", factory.Options{...})
//	// or via env:
//	tr, err := factory.NewFromEnv(":3000", factory.Options{...})
package factory

import (
	"crypto/tls"
	"fmt"
	"os"

	peer2peer "github.com/Faizan2005/DFS-Go/Peer2Peer"
	quictransport "github.com/Faizan2005/DFS-Go/Peer2Peer/quic"
)

// newTCPTransport wraps the existing TCPTransport for use via the factory.
func newTCPTransport(listenAddr string, opts Options) peer2peer.Transport {
	return peer2peer.NewTCPTransport(peer2peer.TCPTransportOpts{
		ListenAddr:       listenAddr,
		HandshakeFunc:    peer2peer.NOPEHandshakeFunc,
		Decoder:          peer2peer.DefaultDecoder{},
		TLSConfig:        opts.TLSConfig,
		OnPeer:           opts.OnPeer,
		OnPeerDisconnect: opts.OnPeerDisconnect,
	})
}

// Protocol selects the underlying transport.
type Protocol string

const (
	ProtocolQUIC Protocol = "quic"
	ProtocolTCP  Protocol = "tcp"

	// DefaultProtocol is QUIC — better for cross-subnet college deployments.
	DefaultProtocol Protocol = ProtocolQUIC

	// EnvKey is the environment variable that overrides the default.
	EnvKey = "DFS_TRANSPORT"
)

// Options are common transport configuration knobs.
type Options struct {
	// TLSConfig is used by both TCP (mTLS) and QUIC transports.
	// When nil, QUIC auto-generates a self-signed cert; TCP runs plaintext.
	TLSConfig *tls.Config

	OnPeer           func(peer2peer.Peer) error
	OnPeerDisconnect func(peer2peer.Peer)
}

// New creates a Transport for the given protocol and listen address.
// Returns an error for unknown protocols.
func New(protocol Protocol, listenAddr string, opts Options) (peer2peer.Transport, error) {
	switch protocol {
	case ProtocolQUIC:
		return quictransport.New(quictransport.TransportOpts{
			ListenAddr:       listenAddr,
			TLSConfig:        opts.TLSConfig,
			OnPeer:           opts.OnPeer,
			OnPeerDisconnect: opts.OnPeerDisconnect,
		})

	case ProtocolTCP:
		return newTCPTransport(listenAddr, opts), nil

	default:
		return nil, fmt.Errorf("factory: unknown protocol %q (use %q or %q)",
			protocol, ProtocolQUIC, ProtocolTCP)
	}
}

// NewFromEnv reads DFS_TRANSPORT from the environment (defaults to "quic")
// and calls New accordingly.
func NewFromEnv(listenAddr string, opts Options) (peer2peer.Transport, error) {
	proto := Protocol(os.Getenv(EnvKey))
	if proto == "" {
		proto = DefaultProtocol
	}
	return New(proto, listenAddr, opts)
}

// ProtocolFromEnv returns the effective protocol without constructing a transport.
func ProtocolFromEnv() Protocol {
	if p := os.Getenv(EnvKey); p != "" {
		return Protocol(p)
	}
	return DefaultProtocol
}
