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
	case opcodeECDHUpload:
		handleECDHUpload(conn, s)
	case opcodeECDHDownload:
		handleECDHDownload(conn, s)
	case opcodeGetManifest:
		handleGetManifestInfo(conn, s)
	case opcodeResolveAlias:
		handleResolveAlias(conn, s)
	default:
		log.Printf("IPC: unknown opcode 0x%02x", opcode[0])
	}
}

// handleUpload handles plaintext (no encryption) uploads.
func handleUpload(conn net.Conn, s *server.Server) {
	key, fileSize, err := readUploadRequest(conn)
	if err != nil {
		log.Println("IPC upload: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	var body io.Reader = conn
	if fileSize > 0 {
		body = io.LimitReader(conn, fileSize)
	}

	if err := s.StoreData(key, body, nil); err != nil {
		log.Println("IPC upload: StoreData:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeStatus(conn, statusOK, fmt.Sprintf("stored %d bytes under key %q", fileSize, key))
}

// handleECDHUpload handles ECDH-encrypted uploads.
func handleECDHUpload(conn net.Conn, s *server.Server) {
	req, err := readECDHUploadRequest(conn)
	if err != nil {
		log.Println("IPC ECDH upload: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	var body io.Reader = conn
	if req.FileSize > 0 {
		body = io.LimitReader(conn, req.FileSize)
	}

	enc := &server.EncryptionMeta{
		DEK:           req.DEK,
		OwnerPubKey:   req.OwnerPubKey,
		OwnerEdPubKey: req.OwnerEdPubKey,
		AccessList:    req.AccessList,
		Signature:     req.Signature,
	}
	if err := s.StoreData(req.StorageKey, body, enc); err != nil {
		log.Println("IPC ECDH upload: StoreData:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeStatus(conn, statusOK, fmt.Sprintf("stored %d bytes (encrypted) under key %q", req.FileSize, req.StorageKey))
}

// handleDownload handles plaintext (no encryption) downloads.
func handleDownload(conn net.Conn, s *server.Server) {
	key, err := readDownloadRequest(conn)
	if err != nil {
		log.Println("IPC download: read header:", err)
		writeDownloadError(conn, "bad request: "+err.Error())
		return
	}

	reader, err := s.GetData(key, nil)
	if err != nil {
		log.Println("IPC download: GetData:", err)
		writeDownloadError(conn, err.Error())
		return
	}

	contentLen := s.ContentLength(key)
	writeDownloadResponse(conn, contentLen)

	if _, err := io.Copy(conn, reader); err != nil {
		log.Println("IPC download: stream:", err)
	}
}

// handleECDHDownload handles encrypted downloads with a client-provided DEK.
// Wire format is identical to the old CSE download: [2B name-len][name][2B dek-len][dek].
func handleECDHDownload(conn net.Conn, s *server.Server) {
	name, dek, err := readECDHDownloadRequest(conn)
	if err != nil {
		log.Println("IPC ECDH download: read header:", err)
		writeDownloadError(conn, "bad request: "+err.Error())
		return
	}

	reader, err := s.GetData(name, dek)
	if err != nil {
		log.Println("IPC ECDH download: GetData:", err)
		writeDownloadError(conn, err.Error())
		return
	}

	contentLen := s.ContentLength(name)
	writeDownloadResponse(conn, contentLen)

	if _, err := io.Copy(conn, reader); err != nil {
		log.Println("IPC ECDH download: stream:", err)
	}
}

// handleGetManifestInfo returns the ECDH encryption metadata from a file's manifest.
func handleGetManifestInfo(conn net.Conn, s *server.Server) {
	name, err := readDownloadRequest(conn)
	if err != nil {
		log.Println("IPC get-manifest: read header:", err)
		writeDownloadError(conn, "bad request: "+err.Error())
		return
	}

	manifest, ok := s.InspectManifest(name)
	if !ok || manifest == nil {
		writeDownloadError(conn, fmt.Sprintf("no manifest found for %q", name))
		return
	}

	writeECDHManifestInfoResponse(conn, manifest)
}

// handleResolveAlias resolves an alias to fingerprint + public keys via gossip.
func handleResolveAlias(conn net.Conn, s *server.Server) {
	alias, err := readResolveAliasRequest(conn)
	if err != nil {
		log.Println("IPC resolve-alias: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	results := s.LookupAlias(alias)
	writeResolveAliasResponse(conn, results)
}
