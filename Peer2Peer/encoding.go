package peer2peer

import (
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io"
	"log"
)

type Decoder interface {
	Decode(io.Reader, *RPC) error
}

type GOBDecoder struct{}

func (dec GOBDecoder) Decode(w io.Reader, rpc *RPC) error {
	return gob.NewDecoder(w).Decode(rpc)
}

// DefaultDecoder reads length-prefixed frames: [4-byte big-endian length][payload bytes].
// The sender (SendMsg / Send) must write frames in the same format.
// This replaces the old fixed-4096 Read which silently truncated large messages.
type DefaultDecoder struct{}

func (dec DefaultDecoder) Decode(r io.Reader, rpc *RPC) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		log.Printf("Error reading frame length: %+v\n", err)
		return err
	}
	length := binary.BigEndian.Uint32(lenBuf[:])
	if length == 0 {
		return fmt.Errorf("DefaultDecoder: zero-length frame")
	}
	if length > 64*1024*1024 { // 64MB sanity cap
		return fmt.Errorf("DefaultDecoder: frame too large: %d bytes", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		log.Printf("Error reading frame body: %+v\n", err)
		return err
	}
	rpc.Payload = buf
	return nil
}
