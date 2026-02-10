package peer2peer

import "net"

type Peer interface {
	net.Conn
	Send([]byte) error
	CloseStream()
}

type Transport interface {
	Addr() string
	listenAndAccept() error
	Consume() <-chan RPC
	Close() error
	Dial(string) error
}
