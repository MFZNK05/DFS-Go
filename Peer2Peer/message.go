package peer2peer

import (
	"net"
	"sync"
)

const (
	IncomingMessage           = 0x1
	IncomingStream            = 0x2
	IncomingMessageWithStream = 0x3 // framed message + raw stream data; handler gets StreamWg
)

type RPC struct {
	From     net.Addr
	Payload  []byte
	Stream   bool
	StreamWg *sync.WaitGroup // non-nil for stream RPCs; call Done() when finished reading
}
