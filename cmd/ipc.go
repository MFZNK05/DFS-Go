package cmd

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

// Binary IPC protocol over Unix socket.
//
// Upload request  (CLI → daemon):
//   [1B opcode=0x01][2B key-len][key bytes][8B file-size][raw file bytes]
//
// Upload response (daemon → CLI):
//   [1B status: 0x00=ok | 0x01=err][4B msg-len][msg bytes]
//
// Download request (CLI → daemon):
//   [1B opcode=0x02][2B key-len][key bytes]
//
// Download response (daemon → CLI):
//   [1B status][8B content-len][body bytes]
//   On error: status=0x01, content-len = len(errMsg), body = errMsg bytes.
//   On ok:    status=0x00, content-len = file size (0 if unknown), body = raw file stream.

const (
	opcodeUpload   byte = 0x01
	opcodeDownload byte = 0x02
	statusOK       byte = 0x00
	statusError    byte = 0x01
)

// writeUploadRequest sends the upload framing header.
// The caller must stream the file bytes immediately after, then call CloseWrite.
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
// After return, conn is positioned at the start of the raw file bytes.
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

// writeStatus writes a status byte + message to conn (used for upload responses).
func writeStatus(conn net.Conn, status byte, msg string) {
	msgBytes := []byte(msg)
	var mlen [4]byte
	binary.BigEndian.PutUint32(mlen[:], uint32(len(msgBytes)))
	conn.Write([]byte{status})
	conn.Write(mlen[:])
	conn.Write(msgBytes)
}

// readStatus reads a status byte + message from conn (used by upload CLI).
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

// writeDownloadRequest sends the download framing header.
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

// readDownloadRequest reads the download framing header on the daemon side.
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

// writeDownloadResponse writes the ok status + content-length header.
// The caller must stream body bytes immediately after.
func writeDownloadResponse(conn net.Conn, contentLen int64) {
	var cl [8]byte
	binary.BigEndian.PutUint64(cl[:], uint64(contentLen))
	conn.Write([]byte{statusOK})
	conn.Write(cl[:])
}

// writeDownloadError writes an error status + message as the download response.
func writeDownloadError(conn net.Conn, errMsg string) {
	msgBytes := []byte(errMsg)
	var cl [8]byte
	binary.BigEndian.PutUint64(cl[:], uint64(len(msgBytes)))
	conn.Write([]byte{statusError})
	conn.Write(cl[:])
	conn.Write(msgBytes)
}

// readDownloadResponseHeader reads the status byte + content-length from a download response.
// On ok:    ok=true,  contentLen = file size (stream that many bytes from conn).
// On error: ok=false, contentLen = error message length (read that many bytes from conn).
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
