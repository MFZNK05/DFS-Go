package cmd

import "strings"

// socketPath returns the IPC address for a given node port string.
// On Unix: /tmp/hermond-3000.sock (Unix domain socket)
// On Windows: \\.\pipe\hermond-3000 (named pipe)
func socketPath(port string) string {
	// strip everything up to and including the last ':'
	p := port
	if idx := strings.LastIndex(port, ":"); idx >= 0 {
		p = port[idx+1:]
	}
	return platformSocketPath(p)
}

// GetSocketPath returns the IPC address for the default port (:3000).
func GetSocketPath() string {
	return socketPath(":3000")
}

// SocketPath returns the IPC address for a given node port string.
// Exported for use by the TUI package.
func SocketPath(port string) string {
	return socketPath(port)
}
