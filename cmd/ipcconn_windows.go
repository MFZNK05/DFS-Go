//go:build windows

package cmd

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
)

func ipcListen(path string) (net.Listener, error) {
	return winio.ListenPipe(path, nil)
}

func ipcDial(path string) (net.Conn, error) {
	return winio.DialPipe(path, nil)
}

func platformSocketPath(port string) string {
	return fmt.Sprintf(`\\.\pipe\hermod-%s`, port)
}

func cleanupSocket(path string) {
	// Named pipes are virtual — no file on disk to clean up.
}
