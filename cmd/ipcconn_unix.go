//go:build !windows

package cmd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

func ipcListen(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}

func ipcDial(path string) (net.Conn, error) {
	return net.Dial("unix", path)
}

func platformSocketPath(port string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("hermod-%s.sock", port))
}

func cleanupSocket(path string) {
	_ = os.RemoveAll(path)
}
