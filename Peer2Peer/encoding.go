package peer2peer

import (
	"encoding/gob"
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

type DefaultDecoder struct{}

func (dec DefaultDecoder) Decode(w io.Reader, rpc *RPC) error {
	buff := make([]byte, 4096)

	n, err := w.Read(buff)
	if err != nil {
		log.Printf("Error: %+v\n", err)
		return err
	}

	rpc.Payload = buff[:n]

	return nil
}
