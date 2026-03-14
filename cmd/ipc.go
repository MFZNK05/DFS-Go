package cmd

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/Faizan2005/DFS-Go/Storage/chunker"
	"github.com/Faizan2005/DFS-Go/ipc"
)

// Local aliases for brevity — avoids ipc.StatusOK everywhere in handlers.
const (
	opcodeUpload             = ipc.OpUpload
	opcodeDownload           = ipc.OpDownload
	opcodeECDHUpload         = ipc.OpECDHUpload
	opcodeECDHDownload       = ipc.OpECDHDownload
	opcodeGetManifest        = ipc.OpGetManifest
	opcodeResolveAlias       = ipc.OpResolveAlias
	opcodeDownloadToFile     = ipc.OpDownloadToFile
	opcodeECDHDownloadToFile = ipc.OpECDHDownloadToFile
	opcodeDirUpload          = ipc.OpDirUpload
	opcodeDirDownload        = ipc.OpDirDownload
	opcodeECDHDirUpload      = ipc.OpECDHDirUpload
	opcodeECDHDirDownload    = ipc.OpECDHDirDownload
	opcodeListUploads        = ipc.OpListUploads
	opcodeListDownloads      = ipc.OpListDownloads
	opcodeBrowsePeer         = ipc.OpBrowsePeer
	opcodeListPeers          = ipc.OpListPeers
	opcodeNodeStatus         = ipc.OpNodeStatus
	opcodeListTransfers      = ipc.OpListTransfers
	opcodeCancelTransfer     = ipc.OpCancelTransfer
	opcodePauseTransfer      = ipc.OpPauseTransfer
	opcodeResumeTransfer     = ipc.OpResumeTransfer
	opcodeRemoveFile         = ipc.OpRemoveFile
	opcodeSearch             = ipc.OpSearch
	opcodeConnectPeer        = ipc.OpConnectPeer
	opcodeDisconnectPeer     = ipc.OpDisconnectPeer
	opcodeFullBrowse         = ipc.OpFullBrowse
	opcodeSendToPeer         = ipc.OpSendToPeer
	opcodeListInbox          = ipc.OpListInbox
	opcodeUnblockPeer        = ipc.OpUnblockPeer
	opcodeScanLAN            = ipc.OpScanLAN
	opcodeSeedUpload     = ipc.OpSeedUpload
	opcodeSeedECDHUpload = ipc.OpSeedECDHUpload
	opcodeShutdown       = ipc.OpShutdown
	statusOK             = ipc.StatusOK
	statusError          = ipc.StatusError
)

// ── Plaintext upload (0x01) ──────────────────────────────────────────

func readUploadRequest(conn net.Conn) (key string, fileSize int64, public bool, err error) {
	var klenBuf [2]byte
	if _, err = io.ReadFull(conn, klenBuf[:]); err != nil {
		return
	}
	klen := binary.BigEndian.Uint16(klenBuf[:])
	keyBuf := make([]byte, klen)
	if _, err = io.ReadFull(conn, keyBuf); err != nil {
		return
	}
	key = string(keyBuf)
	var szBuf [8]byte
	if _, err = io.ReadFull(conn, szBuf[:]); err != nil {
		return
	}
	fileSize = int64(binary.BigEndian.Uint64(szBuf[:]))
	var pubBuf [1]byte
	if _, err = io.ReadFull(conn, pubBuf[:]); err != nil {
		return
	}
	public = pubBuf[0] == 0x01
	return
}

// ── Delegated to ipc package ─────────────────────────────────────────

func writeStatus(conn net.Conn, status byte, msg string) {
	ipc.WriteStatus(conn, status, msg)
}

func readStatus(conn net.Conn) (ok bool, msg string, err error) {
	return ipc.ReadStatus(conn)
}

func writeString16(conn net.Conn, s string) error {
	return ipc.WriteString16(conn, s)
}

func writeString16NoErr(conn net.Conn, s string) {
	ipc.WriteString16NoErr(conn, s)
}

func readString16(conn net.Conn) (string, error) {
	return ipc.ReadString16(conn)
}

func writeJSONResponse(conn net.Conn, data interface{}) {
	ipc.WriteJSONResponse(conn, data)
}

func readJSONResponse(conn net.Conn) ([]byte, error) {
	return ipc.ReadJSONResponse(conn)
}

func writeDownloadResponse(conn net.Conn, contentLen int64) {
	ipc.WriteDownloadResponse(conn, contentLen)
}

func writeDownloadError(conn net.Conn, errMsg string) {
	ipc.WriteDownloadError(conn, errMsg)
}

func readDownloadResponseHeader(conn net.Conn) (ok bool, contentLen int64, err error) {
	return ipc.ReadDownloadResponseHeader(conn)
}

func writeProgressUpdate(conn net.Conn, completed, total int) {
	ipc.WriteProgressUpdate(conn, completed, total)
}

func readProgressOrStatus(conn net.Conn) (completed, total int, isProgress bool, finalOK bool, msg string, err error) {
	return ipc.ReadProgressOrStatus(conn)
}

func writeDirProgressUpdate(conn net.Conn, fileIdx, fileTotal, chunkIdx, chunkTotal int) {
	ipc.WriteDirProgressUpdate(conn, fileIdx, fileTotal, chunkIdx, chunkTotal)
}

func readDirProgressOrStatus(conn net.Conn) (fileIdx, fileTotal, chunkIdx, chunkTotal int, isProgress bool, finalOK bool, msg string, err error) {
	return ipc.ReadDirProgressOrStatus(conn)
}

func writeTransferID(conn net.Conn, tid string) {
	ipc.WriteTransferID(conn, tid)
}

func readTransferID(conn net.Conn) (string, error) {
	return ipc.ReadTransferID(conn)
}

// ── Plaintext download (0x02) ────────────────────────────────────────

func writeDownloadRequest(conn net.Conn, key string) error {
	keyBytes := []byte(key)
	if len(keyBytes) > 0xFFFF {
		return fmt.Errorf("key too long (max 65535 bytes)")
	}
	if _, err := conn.Write([]byte{opcodeDownload}); err != nil {
		return err
	}
	var klen [2]byte
	binary.BigEndian.PutUint16(klen[:], uint16(len(keyBytes)))
	if _, err := conn.Write(klen[:]); err != nil {
		return err
	}
	_, err := conn.Write(keyBytes)
	return err
}

func readDownloadRequest(conn net.Conn) (key string, err error) {
	var klenBuf [2]byte
	if _, err = io.ReadFull(conn, klenBuf[:]); err != nil {
		return
	}
	klen := binary.BigEndian.Uint16(klenBuf[:])
	keyBuf := make([]byte, klen)
	_, err = io.ReadFull(conn, keyBuf)
	key = string(keyBuf)
	return
}

// ── ECDH Upload (0x03) ───────────────────────────────────────────────

type ecdhUploadRequest = ipc.ECDHUploadRequest

func readECDHUploadRequest(conn net.Conn) (*ecdhUploadRequest, error) {
	storageKey, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("storageKey: %w", err)
	}
	dek := make([]byte, 32)
	if _, err := io.ReadFull(conn, dek); err != nil {
		return nil, fmt.Errorf("dek: %w", err)
	}
	ownerPub, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("ownerPub: %w", err)
	}
	ownerEdPub, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("ownerEdPub: %w", err)
	}
	var countBuf [2]byte
	if _, err := io.ReadFull(conn, countBuf[:]); err != nil {
		return nil, fmt.Errorf("accessCount: %w", err)
	}
	count := int(binary.BigEndian.Uint16(countBuf[:]))
	accessList := make([]chunker.AccessEntry, count)
	for i := 0; i < count; i++ {
		recipPub, err := readString16(conn)
		if err != nil {
			return nil, fmt.Errorf("accessList[%d].recipPub: %w", i, err)
		}
		wrappedDEK, err := readString16(conn)
		if err != nil {
			return nil, fmt.Errorf("accessList[%d].wrappedDEK: %w", i, err)
		}
		accessList[i] = chunker.AccessEntry{
			RecipientPubKey: recipPub,
			WrappedDEK:      wrappedDEK,
		}
	}
	sig, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	var szBuf [8]byte
	if _, err := io.ReadFull(conn, szBuf[:]); err != nil {
		return nil, fmt.Errorf("fileSize: %w", err)
	}
	fileSize := int64(binary.BigEndian.Uint64(szBuf[:]))
	var pubBuf [1]byte
	if _, err := io.ReadFull(conn, pubBuf[:]); err != nil {
		return nil, fmt.Errorf("public: %w", err)
	}
	return &ecdhUploadRequest{
		StorageKey:    storageKey,
		DEK:           dek,
		OwnerPubKey:   ownerPub,
		OwnerEdPubKey: ownerEdPub,
		AccessList:    accessList,
		Signature:     sig,
		FileSize:      fileSize,
		Public:        pubBuf[0] == 0x01,
	}, nil
}

// ── ECDH Download (0x04) ─────────────────────────────────────────────

func writeECDHDownloadRequest(conn net.Conn, name string, dek []byte) error {
	nameBytes := []byte(name)
	if len(nameBytes) > 0xFFFF {
		return fmt.Errorf("name too long (max 65535 bytes)")
	}
	if _, err := conn.Write([]byte{opcodeECDHDownload}); err != nil {
		return err
	}
	var nlen [2]byte
	binary.BigEndian.PutUint16(nlen[:], uint16(len(nameBytes)))
	if _, err := conn.Write(nlen[:]); err != nil {
		return err
	}
	if _, err := conn.Write(nameBytes); err != nil {
		return err
	}
	var dlen [2]byte
	binary.BigEndian.PutUint16(dlen[:], uint16(len(dek)))
	if _, err := conn.Write(dlen[:]); err != nil {
		return err
	}
	_, err := conn.Write(dek)
	return err
}

func readECDHDownloadRequest(conn net.Conn) (name string, dek []byte, err error) {
	var nlenBuf [2]byte
	if _, err = io.ReadFull(conn, nlenBuf[:]); err != nil {
		return
	}
	nlen := binary.BigEndian.Uint16(nlenBuf[:])
	nameBuf := make([]byte, nlen)
	if _, err = io.ReadFull(conn, nameBuf); err != nil {
		return
	}
	name = string(nameBuf)
	var dlenBuf [2]byte
	if _, err = io.ReadFull(conn, dlenBuf[:]); err != nil {
		return
	}
	dlen := binary.BigEndian.Uint16(dlenBuf[:])
	dek = make([]byte, dlen)
	_, err = io.ReadFull(conn, dek)
	return
}

// ── Get Manifest Info (0x05) ─────────────────────────────────────────

type manifestInfoResponse = ipc.ManifestInfoResponse

func writeGetManifestRequest(conn net.Conn, name string) error {
	nameBytes := []byte(name)
	if len(nameBytes) > 0xFFFF {
		return fmt.Errorf("name too long (max 65535 bytes)")
	}
	if _, err := conn.Write([]byte{opcodeGetManifest}); err != nil {
		return err
	}
	var nlen [2]byte
	binary.BigEndian.PutUint16(nlen[:], uint16(len(nameBytes)))
	if _, err := conn.Write(nlen[:]); err != nil {
		return err
	}
	_, err := conn.Write(nameBytes)
	return err
}

func writeManifestInfoResponse(conn net.Conn, manifest *chunker.ChunkManifest, isDirectory bool) {
	encByte := byte(0x00)
	if manifest.Encrypted {
		encByte = 0x01
	}
	dirByte := byte(0x00)
	if isDirectory {
		dirByte = 0x01
	}
	conn.Write([]byte{statusOK, encByte, dirByte})
	if !manifest.Encrypted {
		return
	}
	writeString16NoErr(conn, manifest.OwnerPubKey)
	var count [2]byte
	binary.BigEndian.PutUint16(count[:], uint16(len(manifest.AccessList)))
	conn.Write(count[:])
	for _, entry := range manifest.AccessList {
		writeString16NoErr(conn, entry.RecipientPubKey)
		writeString16NoErr(conn, entry.WrappedDEK)
	}
}

func readManifestInfoResponse(conn net.Conn) (*manifestInfoResponse, error) {
	var header [3]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	if header[0] != statusOK {
		var clBuf [8]byte
		copy(clBuf[:2], header[1:3])
		if _, err := io.ReadFull(conn, clBuf[2:]); err != nil {
			return nil, err
		}
		cl := binary.BigEndian.Uint64(clBuf[:])
		errMsg := make([]byte, cl)
		if _, err := io.ReadFull(conn, errMsg); err != nil {
			return nil, fmt.Errorf("server error (message unreadable: %v)", err)
		}
		return nil, fmt.Errorf("%s", errMsg)
	}
	resp := &manifestInfoResponse{
		Encrypted:   header[1] == 0x01,
		IsDirectory: header[2] == 0x01,
	}
	if !resp.Encrypted {
		return resp, nil
	}
	ownerPub, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("ownerPub: %w", err)
	}
	resp.OwnerPub = ownerPub
	var countBuf [2]byte
	if _, err := io.ReadFull(conn, countBuf[:]); err != nil {
		return nil, fmt.Errorf("accessCount: %w", err)
	}
	count := int(binary.BigEndian.Uint16(countBuf[:]))
	resp.AccessList = make([]chunker.AccessEntry, count)
	for i := 0; i < count; i++ {
		recipPub, err := readString16(conn)
		if err != nil {
			return nil, fmt.Errorf("accessList[%d].recipPub: %w", i, err)
		}
		wrappedDEK, err := readString16(conn)
		if err != nil {
			return nil, fmt.Errorf("accessList[%d].wrappedDEK: %w", i, err)
		}
		resp.AccessList[i] = chunker.AccessEntry{
			RecipientPubKey: recipPub,
			WrappedDEK:      wrappedDEK,
		}
	}
	return resp, nil
}

// ── Resolve Alias (0x06) ─────────────────────────────────────────────

func writeResolveAliasRequest(conn net.Conn, alias string) error {
	if _, err := conn.Write([]byte{opcodeResolveAlias}); err != nil {
		return err
	}
	return writeString16(conn, alias)
}

func readResolveAliasRequest(conn net.Conn) (string, error) {
	return readString16(conn)
}

func writeResolveAliasResponse(conn net.Conn, results []ipc.AliasResult) {
	conn.Write([]byte{statusOK})
	var count [2]byte
	binary.BigEndian.PutUint16(count[:], uint16(len(results)))
	conn.Write(count[:])
	for _, r := range results {
		writeString16NoErr(conn, r.Fingerprint)
		writeString16NoErr(conn, r.X25519PubHex)
		writeString16NoErr(conn, r.Ed25519PubHex)
		writeString16NoErr(conn, r.NodeAddr)
	}
}

func readResolveAliasResponse(conn net.Conn) ([]ipc.AliasResult, error) {
	var statusBuf [1]byte
	if _, err := io.ReadFull(conn, statusBuf[:]); err != nil {
		return nil, err
	}
	if statusBuf[0] != statusOK {
		var mlenBuf [4]byte
		if _, err := io.ReadFull(conn, mlenBuf[:]); err != nil {
			return nil, fmt.Errorf("server error (length unreadable: %v)", err)
		}
		mlen := binary.BigEndian.Uint32(mlenBuf[:])
		msgBuf := make([]byte, mlen)
		if _, err := io.ReadFull(conn, msgBuf); err != nil {
			return nil, fmt.Errorf("server error (message unreadable: %v)", err)
		}
		return nil, fmt.Errorf("%s", msgBuf)
	}
	var countBuf [2]byte
	if _, err := io.ReadFull(conn, countBuf[:]); err != nil {
		return nil, err
	}
	count := int(binary.BigEndian.Uint16(countBuf[:]))
	results := make([]ipc.AliasResult, count)
	for i := 0; i < count; i++ {
		fp, err := readString16(conn)
		if err != nil {
			return nil, err
		}
		x25519, err := readString16(conn)
		if err != nil {
			return nil, err
		}
		ed25519, err := readString16(conn)
		if err != nil {
			return nil, err
		}
		addr, err := readString16(conn)
		if err != nil {
			return nil, err
		}
		results[i] = ipc.AliasResult{
			Fingerprint:   fp,
			X25519PubHex:  x25519,
			Ed25519PubHex: ed25519,
			NodeAddr:      addr,
		}
	}
	return results, nil
}

// ── Download-to-File (0x07) ───────────────────────────────────────────

func writeDownloadToFileRequest(conn net.Conn, key, filePath string) error {
	if _, err := conn.Write([]byte{opcodeDownloadToFile}); err != nil {
		return err
	}
	if err := writeString16(conn, key); err != nil {
		return err
	}
	return writeString16(conn, filePath)
}

func readDownloadToFileRequest(conn net.Conn) (key, filePath string, err error) {
	key, err = readString16(conn)
	if err != nil {
		return
	}
	filePath, err = readString16(conn)
	return
}

// ── ECDH Download-to-File (0x08) ─────────────────────────────────────

func writeECDHDownloadToFileRequest(conn net.Conn, key, filePath string, dek []byte) error {
	if _, err := conn.Write([]byte{opcodeECDHDownloadToFile}); err != nil {
		return err
	}
	if err := writeString16(conn, key); err != nil {
		return err
	}
	if err := writeString16(conn, filePath); err != nil {
		return err
	}
	var dlen [2]byte
	binary.BigEndian.PutUint16(dlen[:], uint16(len(dek)))
	if _, err := conn.Write(dlen[:]); err != nil {
		return err
	}
	_, err := conn.Write(dek)
	return err
}

func readECDHDownloadToFileRequest(conn net.Conn) (key, filePath string, dek []byte, err error) {
	key, err = readString16(conn)
	if err != nil {
		return
	}
	filePath, err = readString16(conn)
	if err != nil {
		return
	}
	var dlenBuf [2]byte
	if _, err = io.ReadFull(conn, dlenBuf[:]); err != nil {
		return
	}
	dlen := binary.BigEndian.Uint16(dlenBuf[:])
	dek = make([]byte, dlen)
	_, err = io.ReadFull(conn, dek)
	return
}

// ── Browse Peer (0x0F) ──────────────────────────────────────────────

func writeBrowsePeerRequest(conn net.Conn, peerAddr string) error {
	if _, err := conn.Write([]byte{opcodeBrowsePeer}); err != nil {
		return err
	}
	return writeString16(conn, peerAddr)
}

func readBrowsePeerRequest(conn net.Conn) (peerAddr string, err error) {
	return readString16(conn)
}

// ── Directory Upload (0x09) ──────────────────────────────────────────

func writeDirUploadRequest(conn net.Conn, key, dirPath string, public bool) error {
	if _, err := conn.Write([]byte{opcodeDirUpload}); err != nil {
		return err
	}
	if err := writeString16(conn, key); err != nil {
		return err
	}
	if err := writeString16(conn, dirPath); err != nil {
		return err
	}
	pubByte := byte(0x00)
	if public {
		pubByte = 0x01
	}
	_, err := conn.Write([]byte{pubByte})
	return err
}

func readDirUploadRequest(conn net.Conn) (key, dirPath string, public bool, err error) {
	key, err = readString16(conn)
	if err != nil {
		return
	}
	dirPath, err = readString16(conn)
	if err != nil {
		return
	}
	var pubBuf [1]byte
	if _, err = io.ReadFull(conn, pubBuf[:]); err != nil {
		return
	}
	public = pubBuf[0] == 0x01
	return
}

// ── Directory Download (0x0A) ────────────────────────────────────────

func writeDirDownloadRequest(conn net.Conn, key, outputDir string) error {
	if _, err := conn.Write([]byte{opcodeDirDownload}); err != nil {
		return err
	}
	if err := writeString16(conn, key); err != nil {
		return err
	}
	return writeString16(conn, outputDir)
}

func readDirDownloadRequest(conn net.Conn) (key, outputDir string, err error) {
	key, err = readString16(conn)
	if err != nil {
		return
	}
	outputDir, err = readString16(conn)
	return
}

// ── ECDH Directory Upload (0x0B) ─────────────────────────────────────

type ecdhDirUploadRequest = ipc.ECDHDirUploadRequest

func writeECDHDirUploadRequest(conn net.Conn, req ecdhDirUploadRequest) error {
	if _, err := conn.Write([]byte{opcodeECDHDirUpload}); err != nil {
		return err
	}
	if err := writeString16(conn, req.StorageKey); err != nil {
		return err
	}
	if err := writeString16(conn, req.DirPath); err != nil {
		return err
	}
	if len(req.DEK) != 32 {
		return fmt.Errorf("DEK must be 32 bytes")
	}
	if _, err := conn.Write(req.DEK); err != nil {
		return err
	}
	if err := writeString16(conn, req.OwnerPubKey); err != nil {
		return err
	}
	if err := writeString16(conn, req.OwnerEdPubKey); err != nil {
		return err
	}
	var count [2]byte
	binary.BigEndian.PutUint16(count[:], uint16(len(req.AccessList)))
	if _, err := conn.Write(count[:]); err != nil {
		return err
	}
	for _, entry := range req.AccessList {
		if err := writeString16(conn, entry.RecipientPubKey); err != nil {
			return err
		}
		if err := writeString16(conn, entry.WrappedDEK); err != nil {
			return err
		}
	}
	if err := writeString16(conn, req.Signature); err != nil {
		return err
	}
	pubByte := byte(0x00)
	if req.Public {
		pubByte = 0x01
	}
	_, err := conn.Write([]byte{pubByte})
	return err
}

func readECDHDirUploadRequest(conn net.Conn) (*ecdhDirUploadRequest, error) {
	storageKey, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("storageKey: %w", err)
	}
	dirPath, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("dirPath: %w", err)
	}
	dek := make([]byte, 32)
	if _, err := io.ReadFull(conn, dek); err != nil {
		return nil, fmt.Errorf("dek: %w", err)
	}
	ownerPub, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("ownerPub: %w", err)
	}
	ownerEdPub, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("ownerEdPub: %w", err)
	}
	var countBuf [2]byte
	if _, err := io.ReadFull(conn, countBuf[:]); err != nil {
		return nil, fmt.Errorf("accessCount: %w", err)
	}
	count := int(binary.BigEndian.Uint16(countBuf[:]))
	accessList := make([]chunker.AccessEntry, count)
	for i := 0; i < count; i++ {
		recipPub, err := readString16(conn)
		if err != nil {
			return nil, err
		}
		wrappedDEK, err := readString16(conn)
		if err != nil {
			return nil, err
		}
		accessList[i] = chunker.AccessEntry{
			RecipientPubKey: recipPub,
			WrappedDEK:      wrappedDEK,
		}
	}
	sig, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	var pubBuf [1]byte
	if _, err := io.ReadFull(conn, pubBuf[:]); err != nil {
		return nil, fmt.Errorf("public: %w", err)
	}
	return &ecdhDirUploadRequest{
		StorageKey:    storageKey,
		DirPath:       dirPath,
		DEK:           dek,
		OwnerPubKey:   ownerPub,
		OwnerEdPubKey: ownerEdPub,
		AccessList:    accessList,
		Signature:     sig,
		Public:        pubBuf[0] == 0x01,
	}, nil
}

// ── ECDH Directory Download (0x0C) ───────────────────────────────────

func writeECDHDirDownloadRequest(conn net.Conn, key, outputDir string, dek []byte) error {
	if _, err := conn.Write([]byte{opcodeECDHDirDownload}); err != nil {
		return err
	}
	if err := writeString16(conn, key); err != nil {
		return err
	}
	if err := writeString16(conn, outputDir); err != nil {
		return err
	}
	var dlen [2]byte
	binary.BigEndian.PutUint16(dlen[:], uint16(len(dek)))
	if _, err := conn.Write(dlen[:]); err != nil {
		return err
	}
	_, err := conn.Write(dek)
	return err
}

func readECDHDirDownloadRequest(conn net.Conn) (key, outputDir string, dek []byte, err error) {
	key, err = readString16(conn)
	if err != nil {
		return
	}
	outputDir, err = readString16(conn)
	if err != nil {
		return
	}
	var dlenBuf [2]byte
	if _, err = io.ReadFull(conn, dlenBuf[:]); err != nil {
		return
	}
	dlen := binary.BigEndian.Uint16(dlenBuf[:])
	dek = make([]byte, dlen)
	_, err = io.ReadFull(conn, dek)
	return
}

// ── Seed In Place Upload (0x20) ─────────────────────────────────────
// Wire: [0x20][2B keyLen][key][2B pathLen][path][8B fileSize][1B public]
// CLI sends the file path — daemon opens it directly (no data over IPC).

func writeSeedUploadRequest(conn net.Conn, key, filePath string, fileSize int64, public bool) error {
	if _, err := conn.Write([]byte{opcodeSeedUpload}); err != nil {
		return err
	}
	if err := writeString16(conn, key); err != nil {
		return err
	}
	if err := writeString16(conn, filePath); err != nil {
		return err
	}
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(fileSize))
	if _, err := conn.Write(sz[:]); err != nil {
		return err
	}
	pubByte := byte(0x00)
	if public {
		pubByte = 0x01
	}
	_, err := conn.Write([]byte{pubByte})
	return err
}

func readSeedUploadRequest(conn net.Conn) (key, filePath string, fileSize int64, public bool, err error) {
	key, err = readString16(conn)
	if err != nil {
		return
	}
	filePath, err = readString16(conn)
	if err != nil {
		return
	}
	var szBuf [8]byte
	if _, err = io.ReadFull(conn, szBuf[:]); err != nil {
		return
	}
	fileSize = int64(binary.BigEndian.Uint64(szBuf[:]))
	var pubBuf [1]byte
	if _, err = io.ReadFull(conn, pubBuf[:]); err != nil {
		return
	}
	public = pubBuf[0] == 0x01
	return
}

// ── Seed In Place ECDH Upload (0x21) ────────────────────────────────
// Wire: [0x21][2B keyLen][key][2B pathLen][path][8B fileSize][1B public]
//       [32B DEK][2B ownerPubLen][ownerPub][2B ownerEdPubLen][ownerEdPub]
//       [2B sigLen][sig][2B accessCount]
//       for each: [2B recipPubLen][recipPub][2B wrappedDEKLen][wrappedDEK]

type seedECDHUploadRequest struct {
	Key           string
	FilePath      string
	FileSize      int64
	DEK           []byte
	OwnerPubKey   string
	OwnerEdPubKey string
	AccessList    []chunker.AccessEntry
	Signature     string
	Public        bool
}

func writeSeedECDHUploadRequest(conn net.Conn, req seedECDHUploadRequest) error {
	if _, err := conn.Write([]byte{opcodeSeedECDHUpload}); err != nil {
		return err
	}
	if err := writeString16(conn, req.Key); err != nil {
		return err
	}
	if err := writeString16(conn, req.FilePath); err != nil {
		return err
	}
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(req.FileSize))
	if _, err := conn.Write(sz[:]); err != nil {
		return err
	}
	pubByte := byte(0x00)
	if req.Public {
		pubByte = 0x01
	}
	if _, err := conn.Write([]byte{pubByte}); err != nil {
		return err
	}
	// DEK (32 bytes)
	if _, err := conn.Write(req.DEK); err != nil {
		return err
	}
	// Owner keys + signature
	if err := writeString16(conn, req.OwnerPubKey); err != nil {
		return err
	}
	if err := writeString16(conn, req.OwnerEdPubKey); err != nil {
		return err
	}
	if err := writeString16(conn, req.Signature); err != nil {
		return err
	}
	// Access list
	var acLen [2]byte
	binary.BigEndian.PutUint16(acLen[:], uint16(len(req.AccessList)))
	if _, err := conn.Write(acLen[:]); err != nil {
		return err
	}
	for _, ae := range req.AccessList {
		if err := writeString16(conn, ae.RecipientPubKey); err != nil {
			return err
		}
		if err := writeString16(conn, ae.WrappedDEK); err != nil {
			return err
		}
	}
	return nil
}

func readSeedECDHUploadRequest(conn net.Conn) (*seedECDHUploadRequest, error) {
	key, err := readString16(conn)
	if err != nil {
		return nil, err
	}
	filePath, err := readString16(conn)
	if err != nil {
		return nil, err
	}
	var szBuf [8]byte
	if _, err := io.ReadFull(conn, szBuf[:]); err != nil {
		return nil, err
	}
	fileSize := int64(binary.BigEndian.Uint64(szBuf[:]))
	var pubBuf [1]byte
	if _, err := io.ReadFull(conn, pubBuf[:]); err != nil {
		return nil, err
	}
	// DEK
	dek := make([]byte, 32)
	if _, err := io.ReadFull(conn, dek); err != nil {
		return nil, err
	}
	ownerPub, err := readString16(conn)
	if err != nil {
		return nil, err
	}
	ownerEdPub, err := readString16(conn)
	if err != nil {
		return nil, err
	}
	sig, err := readString16(conn)
	if err != nil {
		return nil, err
	}
	var acLenBuf [2]byte
	if _, err := io.ReadFull(conn, acLenBuf[:]); err != nil {
		return nil, err
	}
	acLen := binary.BigEndian.Uint16(acLenBuf[:])
	accessList := make([]chunker.AccessEntry, acLen)
	for i := range accessList {
		rp, rpErr := readString16(conn)
		if rpErr != nil {
			return nil, rpErr
		}
		wd, wdErr := readString16(conn)
		if wdErr != nil {
			return nil, wdErr
		}
		accessList[i] = chunker.AccessEntry{
			RecipientPubKey: rp,
			WrappedDEK:      wd,
		}
	}
	return &seedECDHUploadRequest{
		Key:           key,
		FilePath:      filePath,
		FileSize:      fileSize,
		DEK:           dek,
		OwnerPubKey:   ownerPub,
		OwnerEdPubKey: ownerEdPub,
		AccessList:    accessList,
		Signature:     sig,
		Public:        pubBuf[0] == 0x01,
	}, nil
}

// ── Transfer control opcodes ─────────────────────────────────────────

func writeListTransfersRequest(conn net.Conn) error {
	_, err := conn.Write([]byte{opcodeListTransfers})
	return err
}

func writeCancelTransferRequest(conn net.Conn, id string) error {
	if _, err := conn.Write([]byte{opcodeCancelTransfer}); err != nil {
		return err
	}
	return writeString16(conn, id)
}

func readTransferIDRequest(conn net.Conn) (string, error) {
	return readString16(conn)
}

func writePauseTransferRequest(conn net.Conn, id string) error {
	if _, err := conn.Write([]byte{opcodePauseTransfer}); err != nil {
		return err
	}
	return writeString16(conn, id)
}

func writeResumeTransferRequest(conn net.Conn, id string) error {
	if _, err := conn.Write([]byte{opcodeResumeTransfer}); err != nil {
		return err
	}
	return writeString16(conn, id)
}

func writeRemoveFileRequest(conn net.Conn, key string) error {
	if _, err := conn.Write([]byte{opcodeRemoveFile}); err != nil {
		return err
	}
	return writeString16(conn, key)
}

func writeSearchRequest(conn net.Conn, query string) error {
	if _, err := conn.Write([]byte{opcodeSearch}); err != nil {
		return err
	}
	return writeString16(conn, query)
}

