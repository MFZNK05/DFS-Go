package peer2peer

import (
	"io"
	"net"
	"sync"
)

const (
	IncomingMessage           = 0x1
	IncomingStream            = 0x2
	IncomingMessageWithStream = 0x3 // framed message + raw stream data; handler gets StreamWg
)

type RPC struct {
	From         net.Addr
	Peer         Peer            // direct reference to the sending peer; use this for stream I/O
	Payload      []byte
	Stream       bool
	StreamWg     *sync.WaitGroup // non-nil for stream RPCs; call Done() when finished reading
	StreamReader io.Reader       // source for raw stream bytes; QUIC sets this to the quic.Stream, TCP sets it to peer
}
