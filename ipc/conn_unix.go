//go:build !windows

package ipc

import "net"

// Dial connects to the daemon IPC socket (Unix domain socket on Linux/macOS).
func Dial(path string) (net.Conn, error) {
	return net.Dial("unix", path)
}
