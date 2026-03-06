package cmd

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"

	server "github.com/Faizan2005/DFS-Go/Server"
	"github.com/Faizan2005/DFS-Go/Storage/chunker"
)

// Binary IPC protocol over Unix socket.
//
// Opcodes:
//   0x01 — Plaintext upload
//   0x02 — Plaintext download
//   0x03 — ECDH encrypted upload
//   0x04 — ECDH download (raw DEK)
//   0x05 — Get manifest info (ECDH fields)
//   0x06 — Resolve alias → fingerprint + keys

const (
	opcodeUpload       byte = 0x01
	opcodeDownload     byte = 0x02
	opcodeECDHUpload   byte = 0x03
	opcodeECDHDownload byte = 0x04
	opcodeGetManifest  byte = 0x05
	opcodeResolveAlias byte = 0x06
	statusOK           byte = 0x00
	statusError        byte = 0x01
)

// ── Plaintext upload (0x01) ──────────────────────────────────────────

// writeUploadRequest sends the upload framing header.
// Format: [0x01][2B key-len][key][8B file-size]
func writeUploadRequest(conn net.Conn, key string, fileSize int64) error {
	keyBytes := []byte(key)
	if len(keyBytes) > 0xFFFF {
		return fmt.Errorf("key too long (max 65535 bytes)")
	}
	if _, err := conn.Write([]byte{opcodeUpload}); err != nil {
		return err
	}
	var klen [2]byte
	binary.BigEndian.PutUint16(klen[:], uint16(len(keyBytes)))
	if _, err := conn.Write(klen[:]); err != nil {
		return err
	}
	if _, err := conn.Write(keyBytes); err != nil {
		return err
	}
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(fileSize))
	_, err := conn.Write(sz[:])
	return err
}

// readUploadRequest reads the upload framing header on the daemon side.
func readUploadRequest(conn net.Conn) (key string, fileSize int64, err error) {
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
	return
}

// ── Status (upload responses) ────────────────────────────────────────

func writeStatus(conn net.Conn, status byte, msg string) {
	msgBytes := []byte(msg)
	var mlen [4]byte
	binary.BigEndian.PutUint32(mlen[:], uint32(len(msgBytes)))
	conn.Write([]byte{status})
	conn.Write(mlen[:])
	conn.Write(msgBytes)
}

func readStatus(conn net.Conn) (ok bool, msg string, err error) {
	var statusBuf [1]byte
	if _, err = io.ReadFull(conn, statusBuf[:]); err != nil {
		return
	}
	var mlenBuf [4]byte
	if _, err = io.ReadFull(conn, mlenBuf[:]); err != nil {
		return
	}
	mlen := binary.BigEndian.Uint32(mlenBuf[:])
	msgBuf := make([]byte, mlen)
	if _, err = io.ReadFull(conn, msgBuf); err != nil {
		return
	}
	ok = statusBuf[0] == statusOK
	msg = string(msgBuf)
	return
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

func writeDownloadResponse(conn net.Conn, contentLen int64) {
	var cl [8]byte
	binary.BigEndian.PutUint64(cl[:], uint64(contentLen))
	conn.Write([]byte{statusOK})
	conn.Write(cl[:])
}

func writeDownloadError(conn net.Conn, errMsg string) {
	msgBytes := []byte(errMsg)
	var cl [8]byte
	binary.BigEndian.PutUint64(cl[:], uint64(len(msgBytes)))
	conn.Write([]byte{statusError})
	conn.Write(cl[:])
	conn.Write(msgBytes)
}

func readDownloadResponseHeader(conn net.Conn) (ok bool, contentLen int64, err error) {
	var statusBuf [1]byte
	if _, err = io.ReadFull(conn, statusBuf[:]); err != nil {
		return
	}
	var clBuf [8]byte
	if _, err = io.ReadFull(conn, clBuf[:]); err != nil {
		return
	}
	ok = statusBuf[0] == statusOK
	contentLen = int64(binary.BigEndian.Uint64(clBuf[:]))
	return
}

// ── ECDH Upload (0x03) ───────────────────────────────────────────────
//
// Format: [0x03][2B key-len][storageKey]
//         [32B dek]
//         [2B owner-x25519-len][owner_x25519_hex]
//         [2B owner-ed25519-len][owner_ed25519_hex]
//         [2B access-count]
//           for each: [2B recip-pub-len][recip_pub_hex][2B wdek-len][wdek_hex]
//         [2B sig-len][sig_hex]
//         [8B file-size]
//         [raw plaintext bytes]

// ecdhUploadRequest holds all fields parsed from an ECDH upload IPC message.
type ecdhUploadRequest struct {
	StorageKey    string
	DEK           []byte
	OwnerPubKey   string
	OwnerEdPubKey string
	AccessList    []chunker.AccessEntry
	Signature     string
	FileSize      int64
}

func writeECDHUploadRequest(conn net.Conn, req ecdhUploadRequest) error {
	keyBytes := []byte(req.StorageKey)
	if len(keyBytes) > 0xFFFF {
		return fmt.Errorf("key too long (max 65535 bytes)")
	}
	if _, err := conn.Write([]byte{opcodeECDHUpload}); err != nil {
		return err
	}
	// storageKey
	if err := writeString16(conn, req.StorageKey); err != nil {
		return err
	}
	// 32-byte DEK
	if len(req.DEK) != 32 {
		return fmt.Errorf("DEK must be 32 bytes, got %d", len(req.DEK))
	}
	if _, err := conn.Write(req.DEK); err != nil {
		return err
	}
	// owner X25519 pub
	if err := writeString16(conn, req.OwnerPubKey); err != nil {
		return err
	}
	// owner Ed25519 pub
	if err := writeString16(conn, req.OwnerEdPubKey); err != nil {
		return err
	}
	// access list
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
	// signature
	if err := writeString16(conn, req.Signature); err != nil {
		return err
	}
	// file size
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(req.FileSize))
	_, err := conn.Write(sz[:])
	return err
}

func readECDHUploadRequest(conn net.Conn) (*ecdhUploadRequest, error) {
	// storageKey
	storageKey, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("storageKey: %w", err)
	}
	// 32-byte DEK
	dek := make([]byte, 32)
	if _, err := io.ReadFull(conn, dek); err != nil {
		return nil, fmt.Errorf("dek: %w", err)
	}
	// owner X25519 pub
	ownerPub, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("ownerPub: %w", err)
	}
	// owner Ed25519 pub
	ownerEdPub, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("ownerEdPub: %w", err)
	}
	// access list
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
	// signature
	sig, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	// file size
	var szBuf [8]byte
	if _, err := io.ReadFull(conn, szBuf[:]); err != nil {
		return nil, fmt.Errorf("fileSize: %w", err)
	}
	fileSize := int64(binary.BigEndian.Uint64(szBuf[:]))

	return &ecdhUploadRequest{
		StorageKey:    storageKey,
		DEK:           dek,
		OwnerPubKey:   ownerPub,
		OwnerEdPubKey: ownerEdPub,
		AccessList:    accessList,
		Signature:     sig,
		FileSize:      fileSize,
	}, nil
}

// ── ECDH Download (0x04) ─────────────────────────────────────────────
// Format: [0x04][2B name-len][name][2B dek-len][dek]

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
// Request:  [0x05][2B key-len][storageKey]
// Response: [status][1B encrypted: 0x00=plain, 0x01=ecdh]
//   If encrypted=0x01:
//     [2B owner-pub-len][owner_x25519_hex]
//     [2B access-count]
//       for each: [2B recip-len][recip_hex][2B wdek-len][wdek_hex]

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

// manifestInfoResponse holds ECDH manifest fields returned to the CLI.
type manifestInfoResponse struct {
	Encrypted  bool
	OwnerPub   string
	AccessList []chunker.AccessEntry
}

func writeECDHManifestInfoResponse(conn net.Conn, manifest *chunker.ChunkManifest) {
	encByte := byte(0x00)
	if manifest.Encrypted {
		encByte = 0x01
	}
	conn.Write([]byte{statusOK, encByte})
	if !manifest.Encrypted {
		return
	}
	// owner X25519 pub
	writeString16NoErr(conn, manifest.OwnerPubKey)
	// access list
	var count [2]byte
	binary.BigEndian.PutUint16(count[:], uint16(len(manifest.AccessList)))
	conn.Write(count[:])
	for _, entry := range manifest.AccessList {
		writeString16NoErr(conn, entry.RecipientPubKey)
		writeString16NoErr(conn, entry.WrappedDEK)
	}
}

func readManifestInfoResponse(conn net.Conn) (*manifestInfoResponse, error) {
	var header [2]byte // [status][encrypted]
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	if header[0] != statusOK {
		// Read error message from download error format
		var clBuf [8]byte
		io.ReadFull(conn, clBuf[:]) //nolint:errcheck
		cl := binary.BigEndian.Uint64(clBuf[:])
		errMsg := make([]byte, cl)
		io.ReadFull(conn, errMsg) //nolint:errcheck
		return nil, fmt.Errorf("%s", errMsg)
	}

	resp := &manifestInfoResponse{
		Encrypted: header[1] == 0x01,
	}
	if !resp.Encrypted {
		return resp, nil
	}

	// owner pub
	ownerPub, err := readString16(conn)
	if err != nil {
		return nil, fmt.Errorf("ownerPub: %w", err)
	}
	resp.OwnerPub = ownerPub

	// access list
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
// Request:  [0x06][2B alias-len][alias]
// Response: [status][2B count]
//   for each: [2B fp-len][fingerprint][2B x25519-len][x25519_hex]
//             [2B ed25519-len][ed25519_hex][2B addr-len][addr]

func writeResolveAliasRequest(conn net.Conn, alias string) error {
	if _, err := conn.Write([]byte{opcodeResolveAlias}); err != nil {
		return err
	}
	return writeString16(conn, alias)
}

func readResolveAliasRequest(conn net.Conn) (string, error) {
	return readString16(conn)
}

func writeResolveAliasResponse(conn net.Conn, results []server.AliasResult) {
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

func readResolveAliasResponse(conn net.Conn) ([]server.AliasResult, error) {
	var statusBuf [1]byte
	if _, err := io.ReadFull(conn, statusBuf[:]); err != nil {
		return nil, err
	}
	if statusBuf[0] != statusOK {
		var mlenBuf [4]byte
		io.ReadFull(conn, mlenBuf[:]) //nolint:errcheck
		mlen := binary.BigEndian.Uint32(mlenBuf[:])
		msgBuf := make([]byte, mlen)
		io.ReadFull(conn, msgBuf) //nolint:errcheck
		return nil, fmt.Errorf("%s", msgBuf)
	}
	var countBuf [2]byte
	if _, err := io.ReadFull(conn, countBuf[:]); err != nil {
		return nil, err
	}
	count := int(binary.BigEndian.Uint16(countBuf[:]))
	results := make([]server.AliasResult, count)
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
		results[i] = server.AliasResult{
			Fingerprint:   fp,
			X25519PubHex:  x25519,
			Ed25519PubHex: ed25519,
			NodeAddr:      addr,
		}
	}
	return results, nil
}

// ── String helpers ───────────────────────────────────────────────────

func writeString16(conn net.Conn, s string) error {
	b := []byte(s)
	if len(b) > 0xFFFF {
		return fmt.Errorf("string too long (max 65535 bytes)")
	}
	var slen [2]byte
	binary.BigEndian.PutUint16(slen[:], uint16(len(b)))
	if _, err := conn.Write(slen[:]); err != nil {
		return err
	}
	_, err := conn.Write(b)
	return err
}

func writeString16NoErr(conn net.Conn, s string) {
	_ = writeString16(conn, s)
}

func readString16(conn net.Conn) (string, error) {
	var slenBuf [2]byte
	if _, err := io.ReadFull(conn, slenBuf[:]); err != nil {
		return "", err
	}
	slen := binary.BigEndian.Uint16(slenBuf[:])
	buf := make([]byte, slen)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
