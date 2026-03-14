package cmd

import (
	"fmt"
	"net"
	"time"

	"github.com/Faizan2005/DFS-Go/ipc"
	"github.com/spf13/cobra"
)

var stopNode string

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a running Hermod daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		sock := socketPath(stopNode)

		conn, err := ipcDialTimeout(sock, 500*time.Millisecond)
		if err != nil {
			return fmt.Errorf("no daemon running on %s (socket: %s)", stopNode, sock)
		}
		defer conn.Close()

		// Send shutdown opcode.
		if _, err := conn.Write([]byte{ipc.OpShutdown}); err != nil {
			return fmt.Errorf("send shutdown: %w", err)
		}

		// Read response (best-effort — daemon may exit before we read).
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		ok, msg, err := readStatus(conn)
		if err != nil {
			// Daemon likely already exited — that's fine.
			fmt.Println("Hermod daemon stopped.")
			return nil
		}
		if !ok {
			return fmt.Errorf("shutdown rejected: %s", msg)
		}
		fmt.Println("Hermod daemon stopped.")
		return nil
	},
}

// ipcDialTimeout wraps ipcDial with a connect timeout for stale socket detection.
func ipcDialTimeout(path string, timeout time.Duration) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, e := ipcDial(path)
		ch <- result{c, e}
	}()
	select {
	case r := <-ch:
		return r.conn, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("connection timed out")
	}
}

func init() {
	stopCmd.Flags().StringVarP(&stopNode, "node", "n", ":3000", "Daemon port to stop")
	rootCmd.AddCommand(stopCmd)
}
