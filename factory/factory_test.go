package factory_test

import (
	"os"
	"testing"

	"github.com/Faizan2005/DFS-Go/factory"
	peer2peer "github.com/Faizan2005/DFS-Go/Peer2Peer"
)

// compile-time guard: factory.New must return a peer2peer.Transport.
var _ peer2peer.Transport

func TestNewQUICTransport(t *testing.T) {
	tr, err := factory.New(factory.ProtocolQUIC, ":0", factory.Options{})
	if err != nil {
		t.Fatalf("expected QUIC transport, got error: %v", err)
	}
	if tr == nil {
		t.Fatal("expected non-nil transport")
	}
}

func TestNewTCPTransport(t *testing.T) {
	tr, err := factory.New(factory.ProtocolTCP, ":0", factory.Options{})
	if err != nil {
		t.Fatalf("expected TCP transport, got error: %v", err)
	}
	if tr == nil {
		t.Fatal("expected non-nil transport")
	}
}

func TestNewUnknownProtocol(t *testing.T) {
	_, err := factory.New("udp", ":0", factory.Options{})
	if err == nil {
		t.Fatal("expected error for unknown protocol, got nil")
	}
}

func TestNewFromEnvDefaultsToQUIC(t *testing.T) {
	os.Unsetenv(factory.EnvKey)
	tr, err := factory.NewFromEnv(":0", factory.Options{})
	if err != nil {
		t.Fatalf("NewFromEnv default: %v", err)
	}
	if tr == nil {
		t.Fatal("expected non-nil transport")
	}
	if factory.ProtocolFromEnv() != factory.ProtocolQUIC {
		t.Errorf("expected default protocol %q, got %q", factory.ProtocolQUIC, factory.ProtocolFromEnv())
	}
}

func TestNewFromEnvTCP(t *testing.T) {
	t.Setenv(factory.EnvKey, string(factory.ProtocolTCP))
	tr, err := factory.NewFromEnv(":0", factory.Options{})
	if err != nil {
		t.Fatalf("NewFromEnv(tcp): %v", err)
	}
	if tr == nil {
		t.Fatal("expected non-nil transport")
	}
	if factory.ProtocolFromEnv() != factory.ProtocolTCP {
		t.Errorf("expected %q, got %q", factory.ProtocolTCP, factory.ProtocolFromEnv())
	}
}

func TestProtocolFromEnvDefault(t *testing.T) {
	os.Unsetenv(factory.EnvKey)
	if got := factory.ProtocolFromEnv(); got != factory.DefaultProtocol {
		t.Errorf("expected DefaultProtocol %q, got %q", factory.DefaultProtocol, got)
	}
}

func TestProtocolFromEnvOverride(t *testing.T) {
	t.Setenv(factory.EnvKey, "tcp")
	if got := factory.ProtocolFromEnv(); got != factory.ProtocolTCP {
		t.Errorf("expected %q, got %q", factory.ProtocolTCP, got)
	}
}
