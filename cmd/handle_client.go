package cmd

import (
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strings"
	"time"

	"os"

	"github.com/Faizan2005/DFS-Go/Cluster/membership"
	server "github.com/Faizan2005/DFS-Go/Server"
	"github.com/Faizan2005/DFS-Go/Server/transfer"
	"github.com/Faizan2005/DFS-Go/State"
	"github.com/Faizan2005/DFS-Go/Storage/chunker"
	"github.com/Faizan2005/DFS-Go/ipc"
)

// recordUploadState records an upload in the StateDB (if available).
func recordUploadState(s *server.Server, key, path string, size int64, encrypted, public, isDir bool) {
	if s.StateDB == nil {
		return
	}
	// Extract name from key (format: fingerprint/name).
	name := key
	if idx := strings.Index(key, "/"); idx >= 0 {
		name = key[idx+1:]
	}
	entry := State.UploadEntry{
		Key:       key,
		Name:      name,
		Path:      path,
		Size:      size,
		Encrypted: encrypted,
		Public:    public,
		IsDir:     isDir,
		Timestamp: time.Now().UnixNano(),
	}
	if err := s.StateDB.RecordUpload(entry); err != nil {
		log.Printf("state: record upload: %v", err)
	}
	if public {
		if err := s.StateDB.AddPublicFile(State.PublicFileEntry{
			Key:       key,
			Name:      name,
			Size:      size,
			IsDir:     isDir,
			Timestamp: time.Now().UnixNano(),
		}); err != nil {
			log.Printf("state: add public file: %v", err)
		}
		s.UpdatePublicCatalogMetadata()
	}
}

// recordDownloadState records a download in the StateDB (if available).
func recordDownloadState(s *server.Server, key, outputPath string, size int64, encrypted, isDir bool) {
	if s.StateDB == nil {
		return
	}
	name := key
	if idx := strings.Index(key, "/"); idx >= 0 {
		name = key[idx+1:]
	}
	entry := State.DownloadEntry{
		Key:        key,
		Name:       name,
		OutputPath: outputPath,
		Size:       size,
		Encrypted:  encrypted,
		IsDir:      isDir,
		Timestamp:  time.Now().UnixNano(),
	}
	if err := s.StateDB.RecordDownload(entry); err != nil {
		log.Printf("state: record download: %v", err)
	}
}

// completeAndRecordTransfer marks a transfer as completed in the TransferMgr
// and persists it to StateDB transfer history.
func completeAndRecordTransfer(s *server.Server, tid string) {
	info, ok := s.TransferMgr.Get(tid)
	if !ok {
		s.TransferMgr.Complete(tid)
		return
	}
	// Read status before Complete() to avoid race with other goroutines.
	status := 3 // completed
	if info.Status == 4 {
		status = 4 // failed
	}
	s.TransferMgr.Complete(tid)
	if s.StateDB == nil {
		return
	}
	entry := State.TransferHistoryEntry{
		ID:        info.ID,
		Direction: int(info.Direction),
		Key:       info.Key,
		Name:      info.Name,
		Size:      info.Size,
		IsDir:     info.IsDir,
		Encrypted: info.Encrypted,
		Public:    info.Public,
		Speed:     info.Speed,
		StartedAt: info.StartedAt.UnixNano(),
		DoneAt:    time.Now().UnixNano(),
		Status:    status,
		Error:     info.Error,
	}
	if err := s.StateDB.RecordTransfer(entry); err != nil {
		log.Printf("state: record transfer: %v", err)
	}
}

// extractName returns the name portion of a "fingerprint/name" key.
func extractName(key string) string {
	if idx := strings.Index(key, "/"); idx >= 0 {
		return key[idx+1:]
	}
	return key
}

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
	case opcodeDownloadToFile:
		handleDownloadToFile(conn, s)
	case opcodeECDHDownloadToFile:
		handleECDHDownloadToFile(conn, s)
	case opcodeDirUpload:
		handleDirUpload(conn, s)
	case opcodeDirDownload:
		handleDirDownload(conn, s)
	case opcodeECDHDirUpload:
		handleECDHDirUpload(conn, s)
	case opcodeECDHDirDownload:
		handleECDHDirDownload(conn, s)
	case opcodeListUploads:
		handleListUploads(conn, s)
	case opcodeListDownloads:
		handleListDownloads(conn, s)
	case opcodeBrowsePeer:
		handleBrowsePeer(conn, s)
	case opcodeListPeers:
		handleListPeers(conn, s)
	case opcodeNodeStatus:
		handleNodeStatus(conn, s)
	case opcodeListTransfers:
		handleListTransfers(conn, s)
	case opcodeCancelTransfer:
		handleCancelTransfer(conn, s)
	case opcodePauseTransfer:
		handlePauseTransfer(conn, s)
	case opcodeResumeTransfer:
		handleResumeTransfer(conn, s)
	case opcodeRemoveFile:
		handleRemoveFile(conn, s)
	case opcodeSearch:
		handleSearch(conn, s)
	case opcodeConnectPeer:
		handleConnectPeer(conn, s)
	case opcodeDisconnectPeer:
		handleDisconnectPeer(conn, s)
	case opcodeFullBrowse:
		handleFullBrowse(conn, s)
	case opcodeSendToPeer:
		handleSendToPeer(conn, s)
	case opcodeListInbox:
		handleListInbox(conn, s)
	case opcodeUnblockPeer:
		handleUnblockPeer(conn, s)
	case opcodeScanLAN:
		handleScanLAN(conn, s)
	case opcodeSeedUpload:
		handleSeedUpload(conn, s)
	case opcodeSeedECDHUpload:
		handleSeedECDHUpload(conn, s)
	case opcodeShutdown:
		handleShutdown(conn, s)
	default:
		log.Printf("IPC: unknown opcode 0x%02x", opcode[0])
	}
}

// handleUpload handles plaintext (no encryption) uploads.
func handleUpload(conn net.Conn, s *server.Server) {


	key, fileSize, public, err := readUploadRequest(conn)
	if err != nil {
		log.Println("IPC upload: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	// Register with TransferMgr.
	tid, ctx, cancel := s.TransferMgr.Register(transfer.Upload, key, extractName(key), "", fileSize, false, false, public)
	defer cancel()
	defer completeAndRecordTransfer(s, tid)
	s.TransferMgr.SetStatus(tid, transfer.Active, "")
	writeTransferID(conn, tid)

	var body io.Reader = conn
	if fileSize > 0 {
		body = io.LimitReader(conn, fileSize)
	}

	progress := func(completed, total int) {
		s.TransferMgr.UpdateProgress(tid, completed, total, int64(completed)*4<<20)
		writeProgressUpdate(conn, completed, total)
	}

	if err := s.StoreDataWithProgress(ctx, key, body, nil, fileSize, progress); err != nil {
		log.Println("IPC upload: StoreData:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}

	recordUploadState(s, key, "", fileSize, false, public, false)

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

	tid, ctx, cancel := s.TransferMgr.Register(transfer.Upload, req.StorageKey, extractName(req.StorageKey), "", req.FileSize, false, true, req.Public)
	defer cancel()
	defer completeAndRecordTransfer(s, tid)
	s.TransferMgr.SetStatus(tid, transfer.Active, "")
	writeTransferID(conn, tid)

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

	progress := func(completed, total int) {
		s.TransferMgr.UpdateProgress(tid, completed, total, int64(completed)*4<<20)
		writeProgressUpdate(conn, completed, total)
	}

	if err := s.StoreDataWithProgress(ctx, req.StorageKey, body, enc, req.FileSize, progress); err != nil {
		log.Println("IPC ECDH upload: StoreData:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}

	recordUploadState(s, req.StorageKey, "", req.FileSize, true, req.Public, false)

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
	defer reader.Close()

	contentLen := s.ContentLength(key)
	writeDownloadResponse(conn, contentLen)

	if _, err := io.Copy(conn, reader); err != nil {
		log.Println("IPC download: stream:", err)
	}

	recordDownloadState(s, key, "", contentLen, false, false)
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
	defer reader.Close()

	contentLen := s.ContentLength(name)
	writeDownloadResponse(conn, contentLen)

	if _, err := io.Copy(conn, reader); err != nil {
		log.Println("IPC ECDH download: stream:", err)
	}

	recordDownloadState(s, name, "", contentLen, true, false)
}

// handleDownloadToFile handles direct-to-disk downloads (opcode 0x07).
// The daemon writes directly to the specified file path using random-access I/O.
// Progress updates are sent back over the IPC socket.
func handleDownloadToFile(conn net.Conn, s *server.Server) {


	key, filePath, err := readDownloadToFileRequest(conn)
	if err != nil {
		log.Println("IPC download-to-file: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	tid, ctx, cancel := s.TransferMgr.Register(transfer.Download, key, extractName(key), filePath, s.ContentLength(key), false, false, false)
	defer cancel()
	defer completeAndRecordTransfer(s, tid)
	s.TransferMgr.SetStatus(tid, transfer.Active, "")
	writeTransferID(conn, tid)

	progress := func(completed, total int) {
		s.TransferMgr.UpdateProgress(tid, completed, total, int64(completed)*4<<20)
		writeProgressUpdate(conn, completed, total)
	}

	if err := s.GetDataToFile(ctx, key, filePath, nil, progress); err != nil {
		log.Println("IPC download-to-file: GetDataToFile:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}

	fileSize := s.ContentLength(key)
	s.TransferMgr.SetSize(tid, fileSize)
	recordDownloadState(s, key, filePath, fileSize, false, false)

	writeStatus(conn, statusOK, fmt.Sprintf("downloaded to %s", filePath))
}

// handleECDHDownloadToFile handles encrypted direct-to-disk downloads (opcode 0x08).
func handleECDHDownloadToFile(conn net.Conn, s *server.Server) {


	key, filePath, dek, err := readECDHDownloadToFileRequest(conn)
	if err != nil {
		log.Println("IPC ECDH download-to-file: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	tid, ctx, cancel := s.TransferMgr.Register(transfer.Download, key, extractName(key), filePath, s.ContentLength(key), false, true, false)
	defer cancel()
	defer completeAndRecordTransfer(s, tid)
	s.TransferMgr.SetStatus(tid, transfer.Active, "")
	writeTransferID(conn, tid)

	progress := func(completed, total int) {
		s.TransferMgr.UpdateProgress(tid, completed, total, int64(completed)*4<<20)
		writeProgressUpdate(conn, completed, total)
	}

	if err := s.GetDataToFile(ctx, key, filePath, dek, progress); err != nil {
		log.Println("IPC ECDH download-to-file: GetDataToFile:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}

	fileSize := s.ContentLength(key)
	s.TransferMgr.SetSize(tid, fileSize)
	recordDownloadState(s, key, filePath, fileSize, true, false)

	writeStatus(conn, statusOK, fmt.Sprintf("downloaded to %s", filePath))
}

// handleGetManifestInfo returns metadata (encryption + type) for a storage key.
// Unified stat: checks directory manifest first, then file manifest — exactly
// one IPC round-trip regardless of whether the key is a file or directory.
func handleGetManifestInfo(conn net.Conn, s *server.Server) {
	name, err := readDownloadRequest(conn)
	if err != nil {
		log.Println("IPC get-manifest: read header:", err)
		writeDownloadError(conn, "bad request: "+err.Error())
		return
	}

	// Check directory manifest first.
	if dm, dmErr := s.GetDirectoryManifest(name); dmErr == nil && dm != nil {
		// Synthesize a ChunkManifest-like response from the DirectoryManifest.
		synthManifest := &chunker.ChunkManifest{
			Encrypted:   dm.Encrypted,
			OwnerPubKey: dm.OwnerPubKey,
			AccessList:  dm.AccessList,
		}
		writeManifestInfoResponse(conn, synthManifest, true)
		return
	}

	// Fall back to file manifest.
	manifest, ok := s.InspectManifest(name)
	if !ok || manifest == nil {
		writeDownloadError(conn, fmt.Sprintf("no manifest found for %q", name))
		return
	}

	writeManifestInfoResponse(conn, manifest, false)
}

// handleResolveAlias resolves an alias to fingerprint + public keys via gossip.
func handleResolveAlias(conn net.Conn, s *server.Server) {
	alias, err := readResolveAliasRequest(conn)
	if err != nil {
		log.Println("IPC resolve-alias: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	srvResults := s.LookupAlias(alias)
	results := make([]ipc.AliasResult, len(srvResults))
	for i, r := range srvResults {
		results[i] = ipc.AliasResult{
			Fingerprint:   r.Fingerprint,
			X25519PubHex:  r.X25519PubHex,
			Ed25519PubHex: r.Ed25519PubHex,
			NodeAddr:      r.NodeAddr,
		}
	}
	writeResolveAliasResponse(conn, results)
}

// handleDirUpload handles plaintext directory uploads (opcode 0x09).
func handleDirUpload(conn net.Conn, s *server.Server) {


	key, dirPath, public, err := readDirUploadRequest(conn)
	if err != nil {
		log.Println("IPC dir-upload: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	tid, ctx, cancel := s.TransferMgr.Register(transfer.Upload, key, extractName(key), dirPath, 0, true, false, public)
	defer cancel()
	defer completeAndRecordTransfer(s, tid)
	s.TransferMgr.SetStatus(tid, transfer.Active, "")
	writeTransferID(conn, tid)

	progress := func(fileIdx, fileTotal, chunkIdx, chunkTotal int) {
		s.TransferMgr.UpdateProgress(tid, fileIdx, fileTotal, 0)
		writeDirProgressUpdate(conn, fileIdx, fileTotal, chunkIdx, chunkTotal)
	}

	if err := s.StoreDirectory(ctx, key, dirPath, nil, progress); err != nil {
		log.Println("IPC dir-upload: StoreDirectory:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}

	var dirSize int64
	if dm, dmErr := s.GetDirectoryManifest(key); dmErr == nil {
		dirSize = dm.TotalSize
	}
	s.TransferMgr.SetSize(tid, dirSize)
	recordUploadState(s, key, dirPath, dirSize, false, public, true)

	writeStatus(conn, statusOK, fmt.Sprintf("stored directory under key %q", key))
}

// handleDirDownload handles plaintext directory downloads (opcode 0x0A).
func handleDirDownload(conn net.Conn, s *server.Server) {


	key, outputDir, err := readDirDownloadRequest(conn)
	if err != nil {
		log.Println("IPC dir-download: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	tid, ctx, cancel := s.TransferMgr.Register(transfer.Download, key, extractName(key), "", 0, true, false, false)
	defer cancel()
	defer completeAndRecordTransfer(s, tid)
	s.TransferMgr.SetStatus(tid, transfer.Active, "")
	s.TransferMgr.SetOutputDir(tid, outputDir)
	writeTransferID(conn, tid)

	progress := func(fileIdx, fileTotal, chunkIdx, chunkTotal int) {
		s.TransferMgr.UpdateProgress(tid, fileIdx, fileTotal, 0)
		writeDirProgressUpdate(conn, fileIdx, fileTotal, chunkIdx, chunkTotal)
	}

	report, err := s.GetDirectory(ctx, key, outputDir, nil, progress)
	if err != nil {
		log.Println("IPC dir-download: GetDirectory:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}

	var dirSize int64
	if dm, dmErr := s.GetDirectoryManifest(key); dmErr == nil {
		dirSize = dm.TotalSize
	}
	s.TransferMgr.SetSize(tid, dirSize)
	recordDownloadState(s, key, outputDir, dirSize, false, true)

	if report.Failed > 0 {
		writeStatus(conn, statusError, fmt.Sprintf("downloaded %d/%d files. %d failed: %v",
			report.Succeeded, report.Succeeded+report.Failed, report.Failed, report.FailedFiles))
		return
	}
	writeStatus(conn, statusOK, fmt.Sprintf("downloaded directory to %s", outputDir))
}

// handleECDHDirUpload handles encrypted directory uploads (opcode 0x0B).
func handleECDHDirUpload(conn net.Conn, s *server.Server) {


	req, err := readECDHDirUploadRequest(conn)
	if err != nil {
		log.Println("IPC ECDH dir-upload: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	tid, ctx, cancel := s.TransferMgr.Register(transfer.Upload, req.StorageKey, extractName(req.StorageKey), req.DirPath, 0, true, true, req.Public)
	defer cancel()
	defer completeAndRecordTransfer(s, tid)
	s.TransferMgr.SetStatus(tid, transfer.Active, "")
	writeTransferID(conn, tid)

	enc := &server.EncryptionMeta{
		DEK:           req.DEK,
		OwnerPubKey:   req.OwnerPubKey,
		OwnerEdPubKey: req.OwnerEdPubKey,
		AccessList:    req.AccessList,
		Signature:     req.Signature,
	}

	progress := func(fileIdx, fileTotal, chunkIdx, chunkTotal int) {
		s.TransferMgr.UpdateProgress(tid, fileIdx, fileTotal, 0)
		writeDirProgressUpdate(conn, fileIdx, fileTotal, chunkIdx, chunkTotal)
	}

	if err := s.StoreDirectory(ctx, req.StorageKey, req.DirPath, enc, progress); err != nil {
		log.Println("IPC ECDH dir-upload: StoreDirectory:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}

	var dirSize int64
	if dm, dmErr := s.GetDirectoryManifest(req.StorageKey); dmErr == nil {
		dirSize = dm.TotalSize
	}
	s.TransferMgr.SetSize(tid, dirSize)
	recordUploadState(s, req.StorageKey, req.DirPath, dirSize, true, req.Public, true)

	writeStatus(conn, statusOK, fmt.Sprintf("stored encrypted directory under key %q", req.StorageKey))
}

// handleECDHDirDownload handles encrypted directory downloads (opcode 0x0C).
func handleECDHDirDownload(conn net.Conn, s *server.Server) {


	key, outputDir, dek, err := readECDHDirDownloadRequest(conn)
	if err != nil {
		log.Println("IPC ECDH dir-download: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	tid, ctx, cancel := s.TransferMgr.Register(transfer.Download, key, extractName(key), "", 0, true, true, false)
	defer cancel()
	defer completeAndRecordTransfer(s, tid)
	s.TransferMgr.SetStatus(tid, transfer.Active, "")
	s.TransferMgr.SetOutputDir(tid, outputDir)
	s.TransferMgr.SetDEK(tid, dek)
	writeTransferID(conn, tid)

	progress := func(fileIdx, fileTotal, chunkIdx, chunkTotal int) {
		s.TransferMgr.UpdateProgress(tid, fileIdx, fileTotal, 0)
		writeDirProgressUpdate(conn, fileIdx, fileTotal, chunkIdx, chunkTotal)
	}

	report, err := s.GetDirectory(ctx, key, outputDir, dek, progress)
	if err != nil {
		log.Println("IPC ECDH dir-download: GetDirectory:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}

	var dirSize int64
	if dm, dmErr := s.GetDirectoryManifest(key); dmErr == nil {
		dirSize = dm.TotalSize
	}
	s.TransferMgr.SetSize(tid, dirSize)
	recordDownloadState(s, key, outputDir, dirSize, true, true)

	if report.Failed > 0 {
		writeStatus(conn, statusError, fmt.Sprintf("downloaded %d/%d files. %d failed: %v",
			report.Succeeded, report.Succeeded+report.Failed, report.Failed, report.FailedFiles))
		return
	}
	writeStatus(conn, statusOK, fmt.Sprintf("downloaded encrypted directory to %s", outputDir))
}

// ── Sprint G: transfer management handlers ──────────────────────────

// handleListTransfers returns active transfers merged with persistent history.
func handleListTransfers(conn net.Conn, s *server.Server) {
	var active []ipc.TransferInfo
	if s.TransferMgr != nil {
		raw := s.TransferMgr.List()
		active = make([]ipc.TransferInfo, len(raw))
		for i, t := range raw {
			active[i] = ipc.TransferInfo{
				ID:        t.ID,
				Direction: int(t.Direction),
				Status:    int(t.Status),
				Key:       t.Key,
				Name:      t.Name,
				Size:      t.Size,
				IsDir:     t.IsDir,
				Encrypted: t.Encrypted,
				Public:    t.Public,
				Completed: t.Completed,
				Total:     t.Total,
				BytesDone: t.BytesDone,
				Speed:     t.Speed,
				StartedAt: t.StartedAt,
				Error:     t.Error,
				FilePath:  t.FilePath,
				OutputDir: t.OutputDir,
			}
		}
	}

	// Merge with persistent history from StateDB.
	if s.StateDB != nil {
		history, err := s.StateDB.ListTransferHistory()
		if err == nil {
			// Build set of active IDs to avoid duplicates.
			activeIDs := make(map[string]bool, len(active))
			for _, a := range active {
				activeIDs[a.ID] = true
			}
			for _, h := range history {
				if activeIDs[h.ID] {
					continue // active entry takes precedence
				}
				active = append(active, ipc.TransferInfo{
					ID:        h.ID,
					Direction: h.Direction,
					Status:    h.Status,
					Key:       h.Key,
					Name:      h.Name,
					Size:      h.Size,
					IsDir:     h.IsDir,
					Encrypted: h.Encrypted,
					Public:    h.Public,
					Completed: 1, // show as fully done
					Total:     1,
					BytesDone: h.Size,
					Speed:     h.Speed,
					StartedAt: time.Unix(0, h.StartedAt),
					Error:     h.Error,
				})
			}
		}
	}

	// Sort: active/queued/paused first, then completed by time (newest first).
	sort.SliceStable(active, func(i, j int) bool {
		iDone := active[i].Status == 3 || active[i].Status == 4
		jDone := active[j].Status == 3 || active[j].Status == 4
		if iDone != jDone {
			return !iDone // active before done
		}
		if iDone && jDone {
			return active[i].StartedAt.After(active[j].StartedAt)
		}
		return false
	})

	writeJSONResponse(conn, active)
}

// handleCancelTransfer cancels a transfer and removes it from the registry.
func handleCancelTransfer(conn net.Conn, s *server.Server) {
	id, err := readTransferIDRequest(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}
	if s.TransferMgr == nil {
		writeStatus(conn, statusError, "no transfer manager")
		return
	}
	if err := s.TransferMgr.Cancel(id); err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeStatus(conn, statusOK, fmt.Sprintf("cancelled transfer %s", id))
}

// handlePauseTransfer pauses a transfer (keeps it in registry for resume).
func handlePauseTransfer(conn net.Conn, s *server.Server) {
	id, err := readTransferIDRequest(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}
	if s.TransferMgr == nil {
		writeStatus(conn, statusError, "no transfer manager")
		return
	}
	if err := s.TransferMgr.Pause(id); err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeStatus(conn, statusOK, fmt.Sprintf("paused transfer %s", id))
}

// handleResumeTransfer resumes a paused transfer by re-spawning its worker.
func handleResumeTransfer(conn net.Conn, s *server.Server) {
	id, err := readTransferIDRequest(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}
	if s.TransferMgr == nil {
		writeStatus(conn, statusError, "no transfer manager")
		return
	}

	// Get transfer info to know what to resume.
	info, ok := s.TransferMgr.Get(id)
	if !ok {
		writeStatus(conn, statusError, fmt.Sprintf("transfer %q not found", id))
		return
	}

	ctx, _, resumeErr := s.TransferMgr.Resume(id)
	if resumeErr != nil {
		writeStatus(conn, statusError, resumeErr.Error())
		return
	}

	// Re-spawn the worker in a background goroutine.
	go func() {
		var opErr error
		switch info.Direction {
		case transfer.Upload:
			if info.IsDir {
				opErr = s.StoreDirectory(ctx, info.Key, info.FilePath, nil, nil)
			} else {
				// File upload resume not supported (data not available).
				opErr = fmt.Errorf("file upload resume not supported")
			}
		case transfer.Download:
			if info.IsDir {
				report, dirErr := s.GetDirectory(ctx, info.Key, info.OutputDir, info.DEK, nil)
				if dirErr != nil {
					opErr = dirErr
				} else if report.Failed > 0 {
					opErr = fmt.Errorf("downloaded %d/%d files, %d failed", report.Succeeded, report.Succeeded+report.Failed, report.Failed)
				}
			} else {
				opErr = s.GetDataToFile(ctx, info.Key, info.FilePath, info.DEK, func(completed, total int) {
					s.TransferMgr.UpdateProgress(id, completed, total, int64(completed)*4<<20)
				})
			}
		}
		if opErr != nil {
			s.TransferMgr.SetStatus(id, transfer.Failed, opErr.Error())
		}
		completeAndRecordTransfer(s, id)
	}()

	writeStatus(conn, statusOK, fmt.Sprintf("resumed transfer %s", id))
}

// handleRemoveFile handles file deletion with tombstoning (Phase 3).
func handleRemoveFile(conn net.Conn, s *server.Server) {
	key, err := readString16(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	propagated, err := s.DeleteFile(key)
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}

	writeStatus(conn, statusOK, fmt.Sprintf("Removed: %s (tombstone propagated to %d peers)", extractName(key), propagated))
}

// handleSearch handles network-wide keyword search (Phase 4).
func handleSearch(conn net.Conn, s *server.Server) {
	query, err := readString16(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	results, err := s.SearchFiles(query)
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeJSONResponse(conn, results)
}

// ── Sprint F: query handlers ────────────────────────────────────────

// handleListUploads returns the upload history as JSON.
func handleListUploads(conn net.Conn, s *server.Server) {
	if s.StateDB == nil {
		writeJSONResponse(conn, []struct{}{})
		return
	}
	uploads, err := s.StateDB.ListUploads()
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeJSONResponse(conn, uploads)
}

// handleListDownloads returns the download history as JSON.
func handleListDownloads(conn net.Conn, s *server.Server) {
	if s.StateDB == nil {
		writeJSONResponse(conn, []struct{}{})
		return
	}
	downloads, err := s.StateDB.ListDownloads()
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeJSONResponse(conn, downloads)
}

// handleBrowsePeer fetches a remote peer's public file catalog via P2P.
func handleBrowsePeer(conn net.Conn, s *server.Server) {
	peerAddr, err := readBrowsePeerRequest(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	catalog, err := s.GetPublicCatalog(peerAddr)
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeJSONResponse(conn, catalog)
}

// peerInfo is the JSON response structure for ListPeers.
type peerInfo struct {
	Addr        string `json:"addr"`
	State       string `json:"state"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Alias       string `json:"alias,omitempty"`
	PublicCount string `json:"publicCount,omitempty"`
}

// handleListPeers returns connected peers with metadata.
// Deduplicates by fingerprint so ephemeral+canonical entries for the same
// node collapse into one (preferring the shorter/canonical address).
func handleListPeers(conn net.Conn, s *server.Server) {
	if s.Cluster == nil {
		writeJSONResponse(conn, []peerInfo{})
		return
	}
	nodes := s.Cluster.AllNodes()
	selfAddr := s.SelfAddr()
	selfFP := s.SelfFingerprint()
	seen := make(map[string]int) // fingerprint → index in peers slice
	peers := make([]peerInfo, 0, len(nodes))
	for _, n := range nodes {
		// Skip self node — match by address or fingerprint to handle
		// address normalization mismatches (e.g. ":3000" vs "127.0.0.1:3000").
		if n.Addr == selfAddr {
			continue
		}
		if selfFP != "" && n.Metadata != nil && n.Metadata["fingerprint"] == selfFP {
			continue
		}
		// Skip Dead/Left nodes — they are gone and clutter the peer list.
		// Also skip Suspect nodes that have no active transport connection
		// (stale gossip entries from peers that already disconnected).
		if n.State == membership.StateDead || n.State == membership.StateLeft {
			continue
		}
		if n.State == membership.StateSuspect && !s.IsPeerConnected(n.Addr) {
			continue
		}
		// Skip entries without a fingerprint — these are ephemeral inbound
		// connection artifacts that haven't been mapped to a canonical address
		// via gossip announce yet. They'll be cleaned up or merged shortly.
		if n.Metadata == nil || n.Metadata["fingerprint"] == "" {
			continue
		}
		p := peerInfo{
			Addr:  n.Addr,
			State: n.State.String(),
		}
		if n.Metadata != nil {
			p.Fingerprint = n.Metadata["fingerprint"]
			p.Alias = n.Metadata["alias"]
			p.PublicCount = n.Metadata["public_shared_count"]
		}
		// Deduplicate by fingerprint: keep the shorter (canonical) address.
		if p.Fingerprint != "" {
			if idx, dup := seen[p.Fingerprint]; dup {
				if len(p.Addr) < len(peers[idx].Addr) {
					peers[idx] = p
				}
				continue
			}
			seen[p.Fingerprint] = len(peers)
		}
		peers = append(peers, p)
	}
	writeJSONResponse(conn, peers)
}

// nodeStatusInfo is the JSON response structure for NodeStatus.
type nodeStatusInfo struct {
	Addr      string `json:"addr"`
	Status    string `json:"status"`
	Uptime    string `json:"uptime"`
	PeerCount int    `json:"peerCount"`
	Uploads   int    `json:"uploads"`
	Downloads int    `json:"downloads"`
}

// handleNodeStatus returns node status summary.
func handleNodeStatus(conn net.Conn, s *server.Server) {
	hs := s.HealthStatus()
	info := nodeStatusInfo{
		Addr:      hs.NodeAddr,
		Status:    hs.Status,
		Uptime:    hs.Uptime,
		PeerCount: hs.PeerCount,
	}
	if s.StateDB != nil {
		if uploads, err := s.StateDB.ListUploads(); err == nil {
			info.Uploads = len(uploads)
		}
		if downloads, err := s.StateDB.ListDownloads(); err == nil {
			info.Downloads = len(downloads)
		}
	}
	writeJSONResponse(conn, info)
}

// ── Seed In Place Upload Handlers ─────────────────────────────────────

// handleSeedUpload handles plaintext seed-in-place uploads.
// The CLI sends the file path — the daemon opens it directly (no data over IPC).
func handleSeedUpload(conn net.Conn, s *server.Server) {


	key, filePath, fileSize, public, err := readSeedUploadRequest(conn)
	if err != nil {
		log.Println("IPC seed upload: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	tid, ctx, cancel := s.TransferMgr.Register(transfer.Upload, key, extractName(key), filePath, fileSize, false, false, public)
	defer cancel()
	defer completeAndRecordTransfer(s, tid)
	s.TransferMgr.SetStatus(tid, transfer.Active, "")
	writeTransferID(conn, tid)

	progress := func(completed, total int) {
		s.TransferMgr.UpdateProgress(tid, completed, total, int64(completed)*4<<20)
		writeProgressUpdate(conn, completed, total)
	}

	if err := s.SeedInPlace(ctx, key, filePath, nil, fileSize, progress); err != nil {
		log.Println("IPC seed upload: SeedInPlace:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}

	recordUploadState(s, key, filePath, fileSize, false, public, false)
	writeStatus(conn, statusOK, fmt.Sprintf("seeded %q (zero-copy, %d bytes indexed)", key, fileSize))
}

// handleSeedECDHUpload handles ECDH-encrypted seed-in-place uploads.
func handleSeedECDHUpload(conn net.Conn, s *server.Server) {


	req, err := readSeedECDHUploadRequest(conn)
	if err != nil {
		log.Println("IPC seed ECDH upload: read header:", err)
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}

	tid, ctx, cancel := s.TransferMgr.Register(transfer.Upload, req.Key, extractName(req.Key), req.FilePath, req.FileSize, false, true, req.Public)
	defer cancel()
	defer completeAndRecordTransfer(s, tid)
	s.TransferMgr.SetStatus(tid, transfer.Active, "")
	writeTransferID(conn, tid)

	enc := &server.EncryptionMeta{
		DEK:           req.DEK,
		OwnerPubKey:   req.OwnerPubKey,
		OwnerEdPubKey: req.OwnerEdPubKey,
		AccessList:    req.AccessList,
		Signature:     req.Signature,
	}

	progress := func(completed, total int) {
		s.TransferMgr.UpdateProgress(tid, completed, total, int64(completed)*4<<20)
		writeProgressUpdate(conn, completed, total)
	}

	if err := s.SeedInPlace(ctx, req.Key, req.FilePath, enc, req.FileSize, progress); err != nil {
		log.Println("IPC seed ECDH upload: SeedInPlace:", err)
		writeStatus(conn, statusError, err.Error())
		return
	}

	recordUploadState(s, req.Key, req.FilePath, req.FileSize, true, req.Public, false)
	writeStatus(conn, statusOK, fmt.Sprintf("seeded (ECDH) %q (zero-copy, %d bytes indexed)", req.Key, req.FileSize))
}

// handleShutdown handles the OpShutdown IPC command.
// Responds OK, then triggers graceful shutdown and exits the daemon process.
func handleShutdown(conn net.Conn, s *server.Server) {
	writeStatus(conn, statusOK, "shutting down")
	conn.Close()
	log.Println("IPC: shutdown requested — stopping daemon")

	// Call the registered shutdown function (set by StartDaemonAsync).
	if fn := getDaemonStopFunc(); fn != nil {
		fn()
	}
	os.Exit(0)
}
