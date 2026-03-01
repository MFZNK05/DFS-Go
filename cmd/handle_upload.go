package cmd

import (
	"fmt"
	"io"
	"net"
	"os"
)

func UploadFile(key, filePath, sockPath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := writeUploadRequest(conn, key, fi.Size()); err != nil {
		return fmt.Errorf("send header: %w", err)
	}

	if _, err := io.Copy(conn, f); err != nil {
		return fmt.Errorf("stream file: %w", err)
	}

	// Signal EOF so the daemon's io.Reader sees end-of-stream.
	if uc, ok := conn.(*net.UnixConn); ok {
		uc.CloseWrite()
	}

	ok, msg, err := readStatus(conn)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if !ok {
		return fmt.Errorf("upload failed: %s", msg)
	}
	fmt.Println("Uploaded:", msg)
	return nil
}
