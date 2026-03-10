// Package quic provides a QUIC-based transport that satisfies the
// peer2peer.Transport interface. It is a drop-in replacement for TCPTransport.
//
// Key differences from TCP:
//   - Each peer connection is a QUIC *Conn over a single UDP socket.
//   - Each logical message (SendMsg / SendStream / Send) opens a new short-lived
//     QUIC stream, writes all bytes in a single writev syscall via net.Buffers,
//     then closes the stream.  This eliminates head-of-line blocking between
//     parallel messages.
//   - TLS 1.3 is built in; a self-signed cert is generated when none provided.
//   - RPC.StreamReader is set to the quic.Stream for IncomingStream and
//     IncomingMessageWithStream RPCs so handlers can read raw bytes without
//     touching the QUICPeer object at all.
package quic

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
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
// It is intentionally minimal — no per-peer mutexes are needed because
// quic.Conn.OpenStreamSync is safe for concurrent use by multiple goroutines.
type QUICPeer struct {
	conn     *quic.Conn
	outbound bool
}

func newQUICPeer(conn *quic.Conn, outbound bool) *QUICPeer {
	return &QUICPeer{conn: conn, outbound: outbound}
}

func (p *QUICPeer) RemoteAddr() net.Addr { return p.conn.RemoteAddr() }
func (p *QUICPeer) LocalAddr() net.Addr  { return p.conn.LocalAddr() }
func (p *QUICPeer) Outbound() bool       { return p.outbound }
func (p *QUICPeer) Close() error         { return p.conn.CloseWithError(0, "peer closed") }
func (p *QUICPeer) CloseStream()         {} // no-op: stream closed by handleStream's defer

// SmoothedRTT returns the QUIC connection's smoothed RTT estimate.
// Satisfies the ratelimit.RTTSource interface.
func (p *QUICPeer) SmoothedRTT() time.Duration {
	return p.conn.ConnectionStats().SmoothedRTT
}

// Read satisfies net.Conn but should not be used directly.
// Use RPC.StreamReader to read stream data in handlers.
func (p *QUICPeer) Read(_ []byte) (int, error) { return 0, io.EOF }

// Write opens a new stream, writes b, closes it.
func (p *QUICPeer) Write(b []byte) (int, error) { return len(b), p.Send(b) }

// Send writes raw bytes on a single QUIC stream.
func (p *QUICPeer) Send(b []byte) error {
	stream, err := p.conn.OpenStreamSync(context.Background())
	if err != nil {
		return err
	}
	defer stream.Close()
	_, err = stream.Write(b)
	return err
}

// SendMsg sends [controlByte][4-byte big-endian len][payload] on one stream.
// Flattened into a single Write call so quic-go packs everything into one UDP
// frame — prevents partial-read splits that break the length-prefixed decoder.
func (p *QUICPeer) SendMsg(controlByte byte, payload []byte) error {
	stream, err := p.conn.OpenStreamSync(context.Background())
	if err != nil {
		return err
	}
	defer stream.Close()

	buf := make([]byte, 1+4+len(payload))
	buf[0] = controlByte
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err = stream.Write(buf)
	return err
}

// SendStream sends [0x3][4-byte len][msgPayload][streamData] on one stream.
// Delegates to SendStreamThrottled with no writer wrapper.
func (p *QUICPeer) SendStream(msgPayload []byte, streamData []byte) error {
	return p.SendStreamThrottled(msgPayload, streamData, nil)
}

// SendStreamThrottled sends a message+stream frame with optional bandwidth
// throttling. The header (control byte + length + msgPayload) is sent at wire
// speed. Stream data is written through wrapWriter (if non-nil) via
// io.CopyBuffer with 32 KiB slices for smooth flow control.
func (p *QUICPeer) SendStreamThrottled(msgPayload, streamData []byte, wrapWriter func(io.Writer) io.Writer) error {
	stream, err := p.conn.OpenStreamSync(context.Background())
	if err != nil {
		return err
	}
	defer stream.Close()

	// Header at wire speed (tiny — control byte + length + msg).
	hdr := make([]byte, 1+4+len(msgPayload))
	hdr[0] = peer2peer.IncomingMessageWithStream
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(msgPayload)))
	copy(hdr[5:], msgPayload)
	if _, err := stream.Write(hdr); err != nil {
		return err
	}

	// Data through optional rate-limited writer (smooth 32 KiB slices).
	var w io.Writer = stream
	if wrapWriter != nil {
		w = wrapWriter(stream)
	}
	buf := make([]byte, 32<<10) // 32 KiB copy buffer
	_, err = io.CopyBuffer(w, bytes.NewReader(streamData), buf)
	return err
}

// Deadline stubs — quic.Conn manages its own timeouts via MaxIdleTimeout.
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
	decoder  peer2peer.Decoder // DefaultDecoder: reads [4-byte len][payload]

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
		opts:    opts,
		rpcCh:   make(chan peer2peer.RPC, 1024),
		tlsCfg:  tlsCfg,
		decoder: peer2peer.DefaultDecoder{},
		peers:   make(map[string]*QUICPeer),
	}, nil
}

func (t *Transport) Addr() string                  { return t.opts.ListenAddr }
func (t *Transport) Consume() <-chan peer2peer.RPC { return t.rpcCh }

func (t *Transport) ListenAndAccept() error {
	cfg := t.tlsCfg.Clone()
	cfg.NextProtos = []string{"dfs-quic"}
	ln, err := quic.ListenAddr(t.opts.ListenAddr, cfg, dfsQUICConfig())
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, addr, cfg, dfsQUICConfig())
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
		// Each stream is handled in its own goroutine — full concurrency,
		// no head-of-line blocking between independent messages.
		go t.handleStream(stream, conn.RemoteAddr(), peer)
	}
}

// handleStream processes a single incoming QUIC stream. It reads the control
// byte, decodes the frame, and dispatches an RPC. For stream RPCs it blocks
// via StreamWg until the handler signals completion, keeping the stream open
// so the handler can read raw bytes via RPC.StreamReader.
func (t *Transport) handleStream(stream *quic.Stream, from net.Addr, peer *QUICPeer) {
	defer stream.Close()

	ctrl := make([]byte, 1)
	if _, err := io.ReadFull(stream, ctrl); err != nil {
		if !isClosedErr(err) {
			log.Printf("[QUIC] read ctrl from %s: %v", from, err)
		}
		return
	}

	switch ctrl[0] {
	case peer2peer.IncomingStream:
		// Standalone stream: handler reads raw bytes via RPC.StreamReader.
		var streamWg sync.WaitGroup
		streamWg.Add(1)
		t.rpcCh <- peer2peer.RPC{
			From:         from,
			Peer:         peer,
			Stream:       true,
			StreamWg:     &streamWg,
			StreamReader: stream, // handler reads from this stream
		}
		streamWg.Wait() // keep stream alive until handler calls streamWg.Done()

	case peer2peer.IncomingMessageWithStream:
		// Framed message + raw stream data on the same stream.
		// Decode the framed message (4-byte len + payload), then expose the
		// remaining stream bytes via StreamReader for the handler to read.
		var msg peer2peer.RPC
		if err := t.decoder.Decode(stream, &msg); err != nil {
			log.Printf("[QUIC] decode msg+stream from %s: %v", from, err)
			return
		}
		msg.From = from
		msg.Peer = peer
		msg.Stream = true
		msg.StreamReader = stream // remaining bytes = raw file data

		var streamWg sync.WaitGroup
		streamWg.Add(1)
		msg.StreamWg = &streamWg
		t.rpcCh <- msg
		streamWg.Wait()

	default:
		// Regular message (0x1): decode framed payload only.
		var msg peer2peer.RPC
		if err := t.decoder.Decode(stream, &msg); err != nil {
			log.Printf("[QUIC] decode msg from %s: %v", from, err)
			return
		}
		msg.From = from
		msg.Peer = peer
		t.rpcCh <- msg
	}
}

// dfsQUICConfig returns the shared quic.Config for both listener and dialer.
// Flow-control windows are sized for 4 MiB chunk transfers:
//   - InitialStreamReceiveWindow  = 8 MiB  (one full chunk + overhead)
//   - InitialConnectionReceiveWindow = 32 MiB (several concurrent streams)
//
// Without these, quic-go defaults (~6 MiB connection window) cause deadlocks
// when multiple large streams contend for receive buffer space.
func dfsQUICConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:                 30 * time.Second,
		KeepAlivePeriod:                10 * time.Second,
		InitialStreamReceiveWindow:     8 << 20,  // 8 MiB
		MaxStreamReceiveWindow:         16 << 20, // 16 MiB
		InitialConnectionReceiveWindow: 32 << 20, // 32 MiB
		MaxConnectionReceiveWindow:     64 << 20, // 64 MiB
	}
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
