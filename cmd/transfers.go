package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/Faizan2005/DFS-Go/ipc"
	"github.com/spf13/cobra"
)

var transfersNode string

var transfersCmd = &cobra.Command{
	Use:   "transfers",
	Short: "List active transfers",
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := net.Dial("unix", socketPath(transfersNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if err := writeListTransfersRequest(conn); err != nil {
			return err
		}

		data, err := readJSONResponse(conn)
		if err != nil {
			return err
		}

		var transfers []ipc.TransferInfo
		if err := json.Unmarshal(data, &transfers); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		if len(transfers) == 0 {
			fmt.Println("No active transfers.")
			return nil
		}

		fmt.Printf("%-10s %-10s %-8s %-20s %-12s %-12s\n", "ID", "DIR", "STATUS", "NAME", "PROGRESS", "SPEED")
		statusNames := []string{"queued", "active", "paused", "completed", "failed"}
		for _, t := range transfers {
			dir := "upload"
			if t.Direction == 1 { // 1 == Download
				dir = "download"
			}

			progress := ""
			if t.Total > 0 {
				progress = fmt.Sprintf("%d/%d", t.Completed, t.Total)
			}

			speed := ""
			if t.Speed > 0 {
				speed = formatSpeed(t.Speed)
			}

			name := t.Name
			if len(name) > 18 {
				name = name[:18] + ".."
			}

			status := "unknown"
			if t.Status >= 0 && t.Status < len(statusNames) {
				status = statusNames[t.Status]
			}

			fmt.Printf("%-10s %-10s %-8s %-20s %-12s %-12s\n",
				t.ID, dir, status, name, progress, speed)
		}
		return nil
	},
}

func formatSpeed(bytesPerSec float64) string {
	switch {
	case bytesPerSec >= 1<<30:
		return fmt.Sprintf("%.1f GB/s", bytesPerSec/float64(1<<30))
	case bytesPerSec >= 1<<20:
		return fmt.Sprintf("%.1f MB/s", bytesPerSec/float64(1<<20))
	case bytesPerSec >= 1<<10:
		return fmt.Sprintf("%.1f KB/s", bytesPerSec/float64(1<<10))
	default:
		return fmt.Sprintf("%.0f B/s", bytesPerSec)
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func init() {
	transfersCmd.Flags().StringVarP(&transfersNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(transfersCmd)
}
