package cmd

import (
	"fmt"
	"strings"
)

// socketPath returns the Unix socket path for a given node port string.
// Port strings are like ":3000" or "127.0.0.1:3000"; we strip the colon/host
// so the socket is /tmp/dfs-3000.sock — one socket per node on this machine.
func socketPath(port string) string {
	// strip everything up to and including the last ':'
	p := port
	if idx := strings.LastIndex(port, ":"); idx >= 0 {
		p = port[idx+1:]
	}
	return fmt.Sprintf("/tmp/dfs-%s.sock", p)
}

// GetSocketPath returns the socket path for the default port (:3000).
// Kept for backward compatibility with any call sites that don't yet have a port.
func GetSocketPath() string {
	return socketPath(":3000")
}

// SocketPath returns the Unix socket path for a given node port string.
// Exported for use by the TUI package.
func SocketPath(port string) string {
	return socketPath(port)
}
