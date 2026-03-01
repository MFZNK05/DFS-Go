package cmd

import (
	"fmt"
	"io"
	"net"
	"os"
)

func DownloadFile(key, outputPath, sockPath string) error {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := writeDownloadRequest(conn, key); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	ok, contentLen, err := readDownloadResponseHeader(conn)
	if err != nil {
		return fmt.Errorf("read response header: %w", err)
	}
	if !ok {
		// Daemon wrote the error message as the body.
		errMsg := make([]byte, contentLen)
		io.ReadFull(conn, errMsg) //nolint:errcheck
		return fmt.Errorf("download failed: %s", errMsg)
	}

	// Determine output destination.
	var out *os.File
	if outputPath == "" {
		out = os.Stdout
	} else {
		out, err = os.Create(outputPath)
		if err != nil {
			return err
		}
		defer out.Close()
	}

	// Stream directly from socket to disk — no intermediate buffer.
	var src io.Reader
	if contentLen > 0 {
		src = io.LimitReader(conn, contentLen)
	} else {
		// contentLen==0 means the daemon doesn't know the size (legacy blob).
		// Read until connection close.
		src = conn
	}

	n, err := io.Copy(out, src)
	if err != nil {
		return fmt.Errorf("stream to disk: %w", err)
	}
	if outputPath != "" {
		fmt.Printf("Downloaded %d bytes → %s\n", n, outputPath)
	}
	return nil
}
