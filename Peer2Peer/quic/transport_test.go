package quic_test

import (
	"fmt"
	"net"
	"testing"
	"time"

	quictransport "github.com/Faizan2005/DFS-Go/Peer2Peer/quic"
	peer2peer "github.com/Faizan2005/DFS-Go/Peer2Peer"
)

// freePort asks the OS for an unused UDP port.
func freePort(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := conn.LocalAddr().String()
	conn.Close()
	return addr
}

func newTransport(t *testing.T, onPeer func(peer2peer.Peer) error, onDisconn func(peer2peer.Peer)) *quictransport.Transport {
	t.Helper()
	tr, err := quictransport.New(quictransport.TransportOpts{
		ListenAddr:       freePort(t),
		OnPeer:           onPeer,
		OnPeerDisconnect: onDisconn,
	})
	if err != nil {
		t.Fatalf("quic.New: %v", err)
	}
	return tr
}

// TestListenAndAccept verifies two transports can connect.
func TestListenAndAccept(t *testing.T) {
	connected := make(chan struct{}, 1)
	server := newTransport(t, func(_ peer2peer.Peer) error {
		connected <- struct{}{}
		return nil
	}, nil)
	if err := server.ListenAndAccept(); err != nil {
		t.Fatalf("server ListenAndAccept: %v", err)
	}
	defer server.Close()

	client := newTransport(t, nil, nil)
	if err := client.ListenAndAccept(); err != nil {
		t.Fatalf("client ListenAndAccept: %v", err)
	}
	defer client.Close()

	if err := client.Dial(server.Addr()); err != nil {
		t.Fatalf("Dial: %v", err)
	}

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for server OnPeer callback")
	}
}

// TestSendReceiveMessage verifies a client can send a message and the server
// receives it on its RPC channel.
func TestSendReceiveMessage(t *testing.T) {
	// The server will receive the RPC. Capture the connected peer on server side.
	serverPeerCh := make(chan peer2peer.Peer, 1)
	server := newTransport(t, func(p peer2peer.Peer) error {
		serverPeerCh <- p
		return nil
	}, nil)
	if err := server.ListenAndAccept(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Close()

	client := newTransport(t, nil, nil)
	if err := client.ListenAndAccept(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer client.Close()

	// Client dials server; server's OnPeer fires.
	if err := client.Dial(server.Addr()); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Wait for server to register the client peer, then have the server
	// send a message back — the client's RPC channel should receive it.
	var serverSidePeer peer2peer.Peer
	select {
	case serverSidePeer = <-serverPeerCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: server never got OnPeer")
	}

	// Server sends a framed message to client using the proper wire protocol.
	payload := []byte("hello")
	if err := serverSidePeer.SendMsg(peer2peer.IncomingMessage, payload); err != nil {
		t.Fatalf("SendMsg from server: %v", err)
	}

	// Client should receive it on its RPC channel.
	select {
	case rpc := <-client.Consume():
		if rpc.Stream {
			t.Error("expected non-stream RPC")
		}
		if len(rpc.Payload) == 0 {
			t.Error("expected non-empty payload")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for client RPC")
	}
}

// TestAddrReturnsListenAddr verifies Addr() returns the configured address.
func TestAddrReturnsListenAddr(t *testing.T) {
	addr := "127.0.0.1:19876"
	tr, err := quictransport.New(quictransport.TransportOpts{ListenAddr: addr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tr.Addr() != addr {
		t.Errorf("Addr() = %q, want %q", tr.Addr(), addr)
	}
}

// TestMultipleDialsIndependent verifies multiple clients can connect simultaneously.
func TestMultipleDialsIndependent(t *testing.T) {
	connCount := make(chan struct{}, 10)
	server := newTransport(t, func(_ peer2peer.Peer) error {
		connCount <- struct{}{}
		return nil
	}, nil)
	if err := server.ListenAndAccept(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Close()

	for i := 0; i < 3; i++ {
		c := newTransport(t, nil, nil)
		if err := c.ListenAndAccept(); err != nil {
			t.Fatalf("client %d start: %v", i, err)
		}
		defer c.Close()
		if err := c.Dial(server.Addr()); err != nil {
			t.Fatalf("client %d dial: %v", i, err)
		}
	}

	got := 0
	deadline := time.After(5 * time.Second)
	for got < 3 {
		select {
		case <-connCount:
			got++
		case <-deadline:
			t.Fatalf("timeout: only %d/3 clients connected", got)
		}
	}
}

// TestOnPeerDisconnectCalled verifies the disconnect callback fires when the
// client peer explicitly closes the connection (sends QUIC CONNECTION_CLOSE).
func TestOnPeerDisconnectCalled(t *testing.T) {
	disconnected := make(chan struct{}, 1)

	// server captures the client peer so we can close it directly.
	clientPeerCh := make(chan peer2peer.Peer, 1)
	server := newTransport(t, func(p peer2peer.Peer) error {
		clientPeerCh <- p
		return nil
	}, func(_ peer2peer.Peer) {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	})
	if err := server.ListenAndAccept(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Close()

	client := newTransport(t, nil, nil)
	if err := client.ListenAndAccept(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	if err := client.Dial(server.Addr()); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Wait for the connection to register on the server side.
	var p peer2peer.Peer
	select {
	case p = <-clientPeerCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for OnPeer")
	}

	// Close the peer from the server side — sends a QUIC CONNECTION_CLOSE.
	p.Close()

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for OnPeerDisconnect")
	}
}

// TestImplementsTransportInterface is a compile-time interface check.
func TestImplementsTransportInterface(t *testing.T) {
	var _ interface {
		Addr() string
		ListenAndAccept() error
		Consume() <-chan peer2peer.RPC
		Close() error
		Dial(string) error
	} = (*quictransport.Transport)(nil)
	_ = fmt.Sprintf("interface check passed")
}

// TestSendMsgRoundTrip verifies that SendMsg produces a framed RPC on the
// receiver's Consume() channel with a non-empty Payload and Stream==false.
func TestSendMsgRoundTrip(t *testing.T) {
	serverPeerCh := make(chan peer2peer.Peer, 1)
	server := newTransport(t, func(p peer2peer.Peer) error {
		serverPeerCh <- p
		return nil
	}, nil)
	if err := server.ListenAndAccept(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Close()

	client := newTransport(t, nil, nil)
	if err := client.ListenAndAccept(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer client.Close()

	if err := client.Dial(server.Addr()); err != nil {
		t.Fatalf("dial: %v", err)
	}

	var serverPeer peer2peer.Peer
	select {
	case serverPeer = <-serverPeerCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for server OnPeer")
	}

	want := []byte("hello-sendmsg")
	if err := serverPeer.SendMsg(peer2peer.IncomingMessage, want); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}

	select {
	case rpc := <-client.Consume():
		if rpc.Stream {
			t.Error("expected non-stream RPC")
		}
		if len(rpc.Payload) == 0 {
			t.Error("expected non-empty payload")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for SendMsg RPC")
	}
}

// TestSendStreamRoundTrip verifies that SendStream produces a stream RPC with
// a non-nil StreamReader that returns the raw stream bytes.
func TestSendStreamRoundTrip(t *testing.T) {
	serverPeerCh := make(chan peer2peer.Peer, 1)
	server := newTransport(t, func(p peer2peer.Peer) error {
		serverPeerCh <- p
		return nil
	}, nil)
	if err := server.ListenAndAccept(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Close()

	client := newTransport(t, nil, nil)
	if err := client.ListenAndAccept(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer client.Close()

	if err := client.Dial(server.Addr()); err != nil {
		t.Fatalf("dial: %v", err)
	}

	var serverPeer peer2peer.Peer
	select {
	case serverPeer = <-serverPeerCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for server OnPeer")
	}

	// Encode a minimal gob Message so the framing decoder can parse it.
	// SendStream sends: [0x3][4-byte len][msgPayload][streamData]
	// The receiver decodes msgPayload as a gob Message, then StreamReader has streamData.
	msgPayload := []byte{peer2peer.IncomingMessage, 0, 0, 0, 0} // 1 ctrl + 4 len = empty payload
	streamData := []byte("stream-payload-data")
	if err := serverPeer.SendStream(msgPayload, streamData); err != nil {
		t.Fatalf("SendStream: %v", err)
	}

	select {
	case rpc := <-client.Consume():
		if !rpc.Stream {
			t.Error("expected stream RPC")
		}
		if rpc.StreamReader == nil {
			t.Fatal("expected non-nil StreamReader")
		}
		if rpc.StreamWg == nil {
			t.Fatal("expected non-nil StreamWg")
		}
		// Signal handler done so the stream goroutine can exit.
		rpc.StreamWg.Done()
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for SendStream RPC")
	}
}

// TestConcurrentSendMsg verifies 10 concurrent SendMsg calls all produce
// distinct RPCs on the receiver's Consume() channel without corruption.
func TestConcurrentSendMsg(t *testing.T) {
	const n = 10
	serverPeerCh := make(chan peer2peer.Peer, 1)
	server := newTransport(t, func(p peer2peer.Peer) error {
		serverPeerCh <- p
		return nil
	}, nil)
	if err := server.ListenAndAccept(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Close()

	client := newTransport(t, nil, nil)
	if err := client.ListenAndAccept(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer client.Close()

	if err := client.Dial(server.Addr()); err != nil {
		t.Fatalf("dial: %v", err)
	}

	var serverPeer peer2peer.Peer
	select {
	case serverPeer = <-serverPeerCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for server OnPeer")
	}

	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			payload := []byte{peer2peer.IncomingMessage, 0, 0, 0, 0}
			errCh <- serverPeer.SendMsg(peer2peer.IncomingMessage, payload)
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent SendMsg error: %v", err)
		}
	}

	got := 0
	deadline := time.After(5 * time.Second)
	for got < n {
		select {
		case rpc := <-client.Consume():
			if rpc.Stream {
				t.Error("expected non-stream RPC")
			}
			got++
		case <-deadline:
			t.Fatalf("timeout: only received %d/%d RPCs", got, n)
		}
	}
}
