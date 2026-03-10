package ipc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// ── String primitives ────────────────────────────────────────────────

// WriteString16 writes [2B len][string] to conn.
func WriteString16(conn net.Conn, s string) error {
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

// WriteString16NoErr writes [2B len][string], ignoring errors.
func WriteString16NoErr(conn net.Conn, s string) {
	_ = WriteString16(conn, s)
}

// ReadString16 reads [2B len][string] from conn.
func ReadString16(conn net.Conn) (string, error) {
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

// ── Status ───────────────────────────────────────────────────────────

// WriteStatus writes [status][4B msg-len][msg] to conn.
func WriteStatus(conn net.Conn, status byte, msg string) {
	msgBytes := []byte(msg)
	var mlen [4]byte
	binary.BigEndian.PutUint32(mlen[:], uint32(len(msgBytes)))
	conn.Write([]byte{status})
	conn.Write(mlen[:])
	conn.Write(msgBytes)
}

// ReadStatus reads [status][4B msg-len][msg] from conn.
func ReadStatus(conn net.Conn) (ok bool, msg string, err error) {
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
	ok = statusBuf[0] == StatusOK
	msg = string(msgBuf)
	return
}

// ── JSON response ────────────────────────────────────────────────────

// WriteJSONResponse writes [status=0x00][4B json-len][json-bytes].
func WriteJSONResponse(conn net.Conn, data interface{}) {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		WriteStatus(conn, StatusError, "json marshal: "+err.Error())
		return
	}
	conn.Write([]byte{StatusOK})
	var mlen [4]byte
	binary.BigEndian.PutUint32(mlen[:], uint32(len(jsonBytes)))
	conn.Write(mlen[:])
	conn.Write(jsonBytes)
}

// ReadJSONResponse reads [status][4B len][json] from conn.
func ReadJSONResponse(conn net.Conn) ([]byte, error) {
	var statusBuf [1]byte
	if _, err := io.ReadFull(conn, statusBuf[:]); err != nil {
		return nil, err
	}
	var mlenBuf [4]byte
	if _, err := io.ReadFull(conn, mlenBuf[:]); err != nil {
		return nil, err
	}
	mlen := binary.BigEndian.Uint32(mlenBuf[:])
	data := make([]byte, mlen)
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, err
	}
	if statusBuf[0] != StatusOK {
		return nil, fmt.Errorf("%s", data)
	}
	return data, nil
}

// ── Download response ────────────────────────────────────────────────

// WriteDownloadResponse writes [statusOK][8B content-len].
func WriteDownloadResponse(conn net.Conn, contentLen int64) {
	var cl [8]byte
	binary.BigEndian.PutUint64(cl[:], uint64(contentLen))
	conn.Write([]byte{StatusOK})
	conn.Write(cl[:])
}

// WriteDownloadError writes [statusError][8B msg-len][msg].
func WriteDownloadError(conn net.Conn, errMsg string) {
	msgBytes := []byte(errMsg)
	var cl [8]byte
	binary.BigEndian.PutUint64(cl[:], uint64(len(msgBytes)))
	conn.Write([]byte{StatusError})
	conn.Write(cl[:])
	conn.Write(msgBytes)
}

// ReadDownloadResponseHeader reads [status][8B content-len].
func ReadDownloadResponseHeader(conn net.Conn) (ok bool, contentLen int64, err error) {
	var statusBuf [1]byte
	if _, err = io.ReadFull(conn, statusBuf[:]); err != nil {
		return
	}
	var clBuf [8]byte
	if _, err = io.ReadFull(conn, clBuf[:]); err != nil {
		return
	}
	ok = statusBuf[0] == StatusOK
	contentLen = int64(binary.BigEndian.Uint64(clBuf[:]))
	return
}

// ── Progress updates ─────────────────────────────────────────────────

// WriteProgressUpdate writes [statusProgress][4B completed][4B total].
func WriteProgressUpdate(conn net.Conn, completed, total int) {
	var buf [9]byte
	buf[0] = StatusProgress
	binary.BigEndian.PutUint32(buf[1:], uint32(completed))
	binary.BigEndian.PutUint32(buf[5:], uint32(total))
	conn.Write(buf[:]) //nolint:errcheck
}

// ReadProgressOrStatus reads either a progress update or final status.
func ReadProgressOrStatus(conn net.Conn) (completed, total int, isProgress bool, finalOK bool, msg string, err error) {
	var statusBuf [1]byte
	if _, err = io.ReadFull(conn, statusBuf[:]); err != nil {
		return
	}
	if statusBuf[0] == StatusProgress {
		var buf [8]byte
		if _, err = io.ReadFull(conn, buf[:]); err != nil {
			return
		}
		completed = int(binary.BigEndian.Uint32(buf[:4]))
		total = int(binary.BigEndian.Uint32(buf[4:]))
		isProgress = true
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
	finalOK = statusBuf[0] == StatusOK
	msg = string(msgBuf)
	return
}

// WriteDirProgressUpdate writes [statusProgress][4B fileIdx][4B fileTotal][4B chunkIdx][4B chunkTotal].
func WriteDirProgressUpdate(conn net.Conn, fileIdx, fileTotal, chunkIdx, chunkTotal int) {
	var buf [17]byte
	buf[0] = StatusProgress
	binary.BigEndian.PutUint32(buf[1:], uint32(fileIdx))
	binary.BigEndian.PutUint32(buf[5:], uint32(fileTotal))
	binary.BigEndian.PutUint32(buf[9:], uint32(chunkIdx))
	binary.BigEndian.PutUint32(buf[13:], uint32(chunkTotal))
	conn.Write(buf[:]) //nolint:errcheck
}

// ReadDirProgressOrStatus reads directory progress or final status.
func ReadDirProgressOrStatus(conn net.Conn) (fileIdx, fileTotal, chunkIdx, chunkTotal int, isProgress bool, finalOK bool, msg string, err error) {
	var statusBuf [1]byte
	if _, err = io.ReadFull(conn, statusBuf[:]); err != nil {
		return
	}
	if statusBuf[0] == StatusProgress {
		var buf [16]byte
		if _, err = io.ReadFull(conn, buf[:]); err != nil {
			return
		}
		fileIdx = int(binary.BigEndian.Uint32(buf[:4]))
		fileTotal = int(binary.BigEndian.Uint32(buf[4:8]))
		chunkIdx = int(binary.BigEndian.Uint32(buf[8:12]))
		chunkTotal = int(binary.BigEndian.Uint32(buf[12:]))
		isProgress = true
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
	finalOK = statusBuf[0] == StatusOK
	msg = string(msgBuf)
	return
}

// ── Transfer ID ──────────────────────────────────────────────────────

// WriteTransferID writes [2B tid-len][tid].
func WriteTransferID(conn net.Conn, tid string) {
	WriteString16NoErr(conn, tid)
}

// ReadTransferID reads [2B tid-len][tid].
func ReadTransferID(conn net.Conn) (string, error) {
	return ReadString16(conn)
}
