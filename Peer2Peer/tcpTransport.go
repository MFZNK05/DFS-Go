package peer2peer

import (
	"errors"
	"log"
	"net"
	"sync"
)

type TCPPeer struct {
	net.Conn
	outbound bool
	Wg       *sync.WaitGroup
}

type TCPTransportOpts struct {
	ListenAddr    string
	HandshakeFunc HandshakeFunc
	Decoder       Decoder
	OnPeer        func(Peer) error
}

type TCPTransport struct {
	TCPTransportOpts
	Listener net.Listener
	rpcch    chan RPC
}

func NewTCPPeer(conn net.Conn, outbound bool) *TCPPeer {
	return &TCPPeer{
		Conn:     conn,
		outbound: outbound,
		Wg:       &sync.WaitGroup{},
	}
}

func NewTCPTransport(opts TCPTransportOpts) *TCPTransport {
	return &TCPTransport{
		TCPTransportOpts: opts,
		rpcch:            make(chan RPC, 1024),
	}
}

func (t *TCPTransport) Addr() string {
	return t.ListenAddr
}

func (t *TCPTransport) Consume() <-chan RPC {
	return t.rpcch
}

func (t *TCPTransport) Close() error {
	if err := t.Listener.Close(); err != nil {
		return err
	}

	return nil
}

func (p *TCPPeer) Close() error {
	return p.Conn.Close()
}

func (t *TCPTransport) ListenAndAccept() error {
	var err error

	t.Listener, err = net.Listen("tcp", t.ListenAddr)
	if err != nil {
		log.Printf("Failed to listen on %s: %v", t.ListenAddr, err)
		return err
	}

	go t.loopAndAccept()

	return nil
}

func (t *TCPTransport) loopAndAccept() {
	for {
		conn, err := t.Listener.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			log.Printf("Error: %+v\n", err)
			continue
		}

		go t.handleConn(conn, false)
	}
}

func (t *TCPTransport) handleConn(conn net.Conn, outbound bool) {
	peer := NewTCPPeer(conn, outbound)
	log.Printf("HANDLE_CONN: New connection from %s (outbound: %v)", conn.RemoteAddr(), outbound)

	defer func() {
		log.Printf("HANDLE_CONN: Closing connection from %s", conn.RemoteAddr())
		conn.Close()
	}()

	log.Println("HANDLE_CONN: Starting handshake...")
	err := t.HandshakeFunc(peer)
	if err != nil {
		log.Printf("HANDLE_CONN: Handshake failed: %v", err)
		return
	}

	if t.OnPeer != nil {
		log.Println("HANDLE_CONN: Calling OnPeer callback...")
		if err := t.OnPeer(peer); err != nil {
			log.Printf("HANDLE_CONN: OnPeer failed: %v", err)
			return
		}
	}

	log.Println("HANDLE_CONN: Starting read loop...")
	for {
		log.Println("HANDLE_CONN: Waiting for message...")
		msg := RPC{}
		msg.From = conn.RemoteAddr()

		err = t.Decoder.Decode(conn, &msg)
		if err != nil {
			log.Printf("HANDLE_CONN: Decode error from %v: %v", conn.RemoteAddr(), err)
			return
		}

		log.Printf("HANDLE_CONN: Received message (stream: %v)", msg.Stream)

		buf := make([]byte, 1)
		_, err := conn.Read(buf)
		if err != nil {
			log.Printf("HANDLE_CONN: Failed to read control byte from %s: %v", conn.RemoteAddr(), err)
			return
		}

		if buf[0] == IncomingStream {
			peer.Wg.Add(1)
			log.Println("HANDLE_CONN: WaitGroup added (count:", peer.Wg, ")")

			log.Println("HANDLE_CONN: Waiting for stream completion...")
			t.rpcch <- msg
			peer.Wg.Wait()
			log.Println("HANDLE_CONN: Stream completed, continuing...")
			continue
		}

		log.Println("HANDLE_CONN: Sending message to channel...")
		t.rpcch <- msg
	}
}

func (t *TCPTransport) Dial(addr string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}

	go t.handleConn(conn, true)

	return nil
}

func (t *TCPPeer) RemoteAddr() net.Addr {
	return t.Conn.RemoteAddr()
}

func (t *TCPPeer) Send(b []byte) error {
	log.Printf("PEER_SEND: Attempting to send %d bytes to %s", len(b), t.RemoteAddr())
	n, err := t.Conn.Write(b)
	if err != nil {
		log.Printf("PEER_SEND: Failed to send: %v", err)
		return err
	}
	log.Printf("PEER_SEND: Successfully sent %d bytes", n)
	return nil
}

func (t *TCPPeer) CloseStream() {
	log.Println("CLOSE_STREAM: Releasing WaitGroup")
	t.Wg.Done()
}
