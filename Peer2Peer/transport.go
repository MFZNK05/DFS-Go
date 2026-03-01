package peer2peer

import "net"

type Peer interface {
	net.Conn
	Send([]byte) error
	// SendMsg atomically sends a control byte followed by a gob-encoded message
	// payload in a single locked section, preventing byte interleaving between
	// concurrent senders on the same connection.
	SendMsg(controlByte byte, payload []byte) error
	// SendStream atomically sends a framed message followed by a stream signal
	// and stream data.  The entire sequence is serialised so concurrent callers
	// on the same connection cannot interleave bytes.
	SendStream(msgPayload []byte, streamData []byte) error
	CloseStream()
	Outbound() bool
}

type Transport interface {
	Addr() string
	ListenAndAccept() error
	Consume() <-chan RPC
	Close() error
	Dial(string) error
}
