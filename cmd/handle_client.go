package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"

	server "github.com/Faizan2005/DFS-Go/Server"
)

type Request struct {
	Action string `json:"action"` // "upload" or "download"
	Key    string `json:"key"`
	Data   string `json:"data,omitempty"` // used only for upload
}

func HandleClient(conn net.Conn, s *server.Server) {
	defer conn.Close()

	buff := make([]byte, 4096)
	n, err := conn.Read(buff)
	if err != nil {
		log.Println("Error reading:", err)
		return
	}

	req := new(Request)
	err = json.Unmarshal(buff[:n], &req)
	if err != nil {
		log.Println("Invalid request:", err)
		return
	}

	switch req.Action {
	case "upload":
		reader := bytes.NewReader([]byte(req.Data))
		s.StoreData(req.Key, reader)
		conn.Write([]byte("uploaded\n"))
	case "download":
		data, err := s.GetData(req.Key)
		if err != nil {
			conn.Write([]byte("error: " + err.Error()))
			return
		}
		b, _ := io.ReadAll(data)
		conn.Write(b)
	default:
		conn.Write([]byte("unknown action\n"))
	}
}
