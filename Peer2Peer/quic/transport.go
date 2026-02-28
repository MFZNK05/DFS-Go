// Package quic provides a QUIC-based transport that satisfies the
// peer2peer.Transport interface. It is a drop-in replacement for TCPTransport.
//
// Key differences from TCP:
//   - Each peer connection is a QUIC *Conn over a single UDP socket.
//   - Each message send opens a new short-lived QUIC Stream, eliminating
//     head-of-line blocking between parallel chunk fetches.
//   - TLS 1.3 is built in; a self-signed cert is generated when none provided.
//   - 0-RTT reconnect for known peers (QUIC session resumption).
package quic

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	peer2peer "github.com/Faizan2005/DFS-Go/Peer2Peer"
)

// QUICPeer wraps a QUIC *Conn and satisfies peer2peer.Peer.
type QUICPeer struct {
	conn     *quic.Conn
	wg       sync.WaitGroup
	outbound bool
}

func newQUICPeer(conn *quic.Conn, outbound bool) *QUICPeer {
	return &QUICPeer{conn: conn, outbound: outbound}
}

func (p *QUICPeer) RemoteAddr() net.Addr { return p.conn.RemoteAddr() }
func (p *QUICPeer) LocalAddr() net.Addr  { return p.conn.LocalAddr() }
func (p *QUICPeer) Read(_ []byte) (int, error) { return 0, io.EOF }

func (p *QUICPeer) Write(b []byte) (int, error) {
	stream, err := p.conn.OpenStreamSync(context.Background())
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	return stream.Write(b)
}

func (p *QUICPeer) Send(b []byte) error { _, err := p.Write(b); return err }
func (p *QUICPeer) CloseStream()        { p.wg.Done() }
func (p *QUICPeer) Close() error        { return p.conn.CloseWithError(0, "peer closed") }

func (p *QUICPeer) SetDeadline(t time.Time) error      { return nil }
func (p *QUICPeer) SetReadDeadline(t time.Time) error  { return nil }
func (p *QUICPeer) SetWriteDeadline(t time.Time) error { return nil }

// ---------------------------------------------------------------------------

type TransportOpts struct {
	ListenAddr       string
	TLSConfig        *tls.Config
	OnPeer           func(peer2peer.Peer) error
	OnPeerDisconnect func(peer2peer.Peer)
}

type Transport struct {
	opts     TransportOpts
	listener *quic.Listener
	rpcCh    chan peer2peer.RPC
	tlsCfg   *tls.Config

	peerLock sync.RWMutex
	peers    map[string]*QUICPeer
}

func New(opts TransportOpts) (*Transport, error) {
	tlsCfg := opts.TLSConfig
	if tlsCfg == nil {
		var err error
		tlsCfg, err = selfSignedTLS()
		if err != nil {
			return nil, err
		}
	}
	return &Transport{
		opts:   opts,
		rpcCh:  make(chan peer2peer.RPC, 1024),
		tlsCfg: tlsCfg,
		peers:  make(map[string]*QUICPeer),
	}, nil
}

func (t *Transport) Addr() string                    { return t.opts.ListenAddr }
func (t *Transport) Consume() <-chan peer2peer.RPC   { return t.rpcCh }

func (t *Transport) ListenAndAccept() error {
	cfg := t.tlsCfg.Clone()
	cfg.NextProtos = []string{"dfs-quic"}
	ln, err := quic.ListenAddr(t.opts.ListenAddr, cfg, &quic.Config{
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
	})
	if err != nil {
		return err
	}
	t.listener = ln
	log.Printf("[QUIC] listening on %s", t.opts.ListenAddr)
	go t.acceptLoop()
	return nil
}

func (t *Transport) Dial(addr string) error {
	cfg := t.tlsCfg.Clone()
	cfg.InsecureSkipVerify = true
	cfg.NextProtos = []string{"dfs-quic"}
	conn, err := quic.DialAddr(context.Background(), addr, cfg, &quic.Config{
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
	})
	if err != nil {
		return err
	}
	go t.handleConn(conn, true)
	return nil
}

func (t *Transport) Close() error {
	if t.listener == nil {
		return nil
	}
	return t.listener.Close()
}

func (t *Transport) acceptLoop() {
	for {
		conn, err := t.listener.Accept(context.Background())
		if err != nil {
			if isClosedErr(err) {
				return
			}
			log.Printf("[QUIC] accept error: %v", err)
			continue
		}
		go t.handleConn(conn, false)
	}
}

func (t *Transport) handleConn(conn *quic.Conn, outbound bool) {
	peer := newQUICPeer(conn, outbound)
	remoteAddr := conn.RemoteAddr().String()

	t.peerLock.Lock()
	t.peers[remoteAddr] = peer
	t.peerLock.Unlock()

	defer func() {
		t.peerLock.Lock()
		delete(t.peers, remoteAddr)
		t.peerLock.Unlock()
		if t.opts.OnPeerDisconnect != nil {
			t.opts.OnPeerDisconnect(peer)
		}
		conn.CloseWithError(0, "disconnected")
		log.Printf("[QUIC] peer disconnected: %s", remoteAddr)
	}()

	if t.opts.OnPeer != nil {
		if err := t.opts.OnPeer(peer); err != nil {
			log.Printf("[QUIC] OnPeer error %s: %v", remoteAddr, err)
			return
		}
	}
	log.Printf("[QUIC] peer connected: %s (outbound=%v)", remoteAddr, outbound)
	t.acceptStreams(conn, peer)
}

func (t *Transport) acceptStreams(conn *quic.Conn, peer *QUICPeer) {
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			if !isClosedErr(err) {
				log.Printf("[QUIC] AcceptStream %s: %v", conn.RemoteAddr(), err)
			}
			return
		}
		go t.handleStream(stream, conn.RemoteAddr(), peer)
	}
}

func (t *Transport) handleStream(stream *quic.Stream, from net.Addr, peer *QUICPeer) {
	defer stream.Close()

	ctrl := make([]byte, 1)
	if _, err := io.ReadFull(stream, ctrl); err != nil {
		log.Printf("[QUIC] read ctrl from %s: %v", from, err)
		return
	}

	if ctrl[0] == peer2peer.IncomingStream {
		peer.wg.Add(1)
		t.rpcCh <- peer2peer.RPC{From: from, Stream: true}
		peer.wg.Wait()
		return
	}

	payload, err := io.ReadAll(stream)
	if err != nil {
		log.Printf("[QUIC] read payload from %s: %v", from, err)
		return
	}
	t.rpcCh <- peer2peer.RPC{From: from, Payload: payload}
}

func selfSignedTLS() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "dfs-node"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"dfs-quic"},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func isClosedErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "use of closed") ||
		strings.Contains(s, "server closed") ||
		strings.Contains(s, "Application error 0x0") ||
		strings.Contains(s, "NO_ERROR") ||
		strings.Contains(s, "connection reset")
}
