//go:build windows

package ipc

import (
	"net"

	"gopkg.in/natefinch/npipe.v2"
)

// Dial connects to the daemon IPC socket (named pipe on Windows).
func Dial(path string) (net.Conn, error) {
	return npipe.Dial(path)
}
