//go:build windows

package ipc

import (
	"net"

	"github.com/Microsoft/go-winio"
)

// Dial connects to the daemon IPC socket (named pipe on Windows).
func Dial(path string) (net.Conn, error) {
	return winio.DialPipe(path, nil)
}
