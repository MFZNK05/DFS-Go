package cmd

import (
	"bytes"
	"encoding/base64"
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

	// Read the full request (no arbitrary size limit)
	data, err := io.ReadAll(conn)
	if err != nil {
		log.Println("Error reading:", err)
		return
	}

	req := new(Request)
	err = json.Unmarshal(data, &req)
	if err != nil {
		log.Println("Invalid request:", err)
		return
	}

	switch req.Action {
	case "upload":
		// Decode base64-encoded file data to preserve binary content
		fileData, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			log.Println("Failed to decode upload data:", err)
			conn.Write([]byte("error: failed to decode upload data: " + err.Error()))
			return
		}
		reader := bytes.NewReader(fileData)
		if err := s.StoreData(req.Key, reader); err != nil {
			log.Println("StoreData failed:", err)
			conn.Write([]byte("error: " + err.Error()))
			return
		}
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
