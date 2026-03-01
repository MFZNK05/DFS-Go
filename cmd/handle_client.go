package cmd

import (
	"fmt"
	"io"
	"log"
	"net"

	server "github.com/Faizan2005/DFS-Go/Server"
)

// HandleClient dispatches a single IPC connection from the CLI.
// Protocol is defined in ipc.go.
func HandleClient(conn net.Conn, s *server.Server) {
	defer conn.Close()

	var opcode [1]byte
	if _, err := io.ReadFull(conn, opcode[:]); err != nil {
		log.Println("IPC: read opcode:", err)
		return
	}

	switch opcode[0] {
	case opcodeUpload:
		handleUpload(conn, s)
	case opcodeDownload:
		handleDownload(conn, s)
	default:
		log.Printf("IPC: unknown opcode 0x%02x", opcode[0])
	}
}

func handleUpload(conn net.Conn, s *server.Server) {
	key, fileSize, err := readUploadRequest(conn)
	if err != nil {
		log.Println("IPC upload: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	// conn is now positioned at the raw file bytes.
	// Use LimitReader when the size is known to guard against a malformed client
	// sending extra bytes; fall back to raw conn (reads until EOF/CloseWrite) otherwise.
	var body io.Reader = conn
	if fileSize > 0 {
		body = io.LimitReader(conn, fileSize)
	}

	if err := s.StoreData(key, body); err != nil {
		log.Println("IPC upload: StoreData:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeStatus(conn, statusOK, fmt.Sprintf("stored %d bytes under key %q", fileSize, key))
}

func handleDownload(conn net.Conn, s *server.Server) {
	key, err := readDownloadRequest(conn)
	if err != nil {
		log.Println("IPC download: read header:", err)
		writeDownloadError(conn, "bad request: "+err.Error())
		return
	}

	reader, err := s.GetData(key)
	if err != nil {
		log.Println("IPC download: GetData:", err)
		writeDownloadError(conn, err.Error())
		return
	}

	// Use manifest TotalSize so the CLI can use LimitReader instead of
	// relying on connection close to signal EOF. Falls back to 0 (unknown)
	// for legacy single-blob files — CLI handles the 0 case by reading until close.
	contentLen := s.ContentLength(key)
	writeDownloadResponse(conn, contentLen)

	if _, err := io.Copy(conn, reader); err != nil {
		log.Println("IPC download: stream:", err)
	}
}
