//go:build windows

package cmd

import (
	"fmt"
	"net"

	"gopkg.in/natefinch/npipe.v2"
)

func ipcListen(path string) (net.Listener, error) {
	return npipe.Listen(path)
}

func ipcDial(path string) (net.Conn, error) {
	return npipe.Dial(path)
}

func platformSocketPath(port string) string {
	return fmt.Sprintf(`\\.\pipe\hermond-%s`, port)
}

func cleanupSocket(path string) {
	// Named pipes are virtual — no file on disk to clean up.
}
