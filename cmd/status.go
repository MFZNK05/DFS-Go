package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

var statusNode string

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show node status",
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := ipcDial(socketPath(statusNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if _, err := conn.Write([]byte{opcodeNodeStatus}); err != nil {
			return err
		}

		data, err := readJSONResponse(conn)
		if err != nil {
			return err
		}

		var info nodeStatusInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		fmt.Printf("Node:      %s\n", info.Addr)
		fmt.Printf("Status:    %s\n", info.Status)
		fmt.Printf("Uptime:    %s\n", info.Uptime)
		fmt.Printf("Peers:     %d\n", info.PeerCount)
		fmt.Printf("Uploads:   %d\n", info.Uploads)
		fmt.Printf("Downloads: %d\n", info.Downloads)
		return nil
	},
}

func init() {
	statusCmd.Flags().StringVarP(&statusNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(statusCmd)
}
