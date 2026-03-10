package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/Faizan2005/DFS-Go/State"
	"github.com/spf13/cobra"
)

var browseNode string

var browseCmd = &cobra.Command{
	Use:   "browse <peer-addr>",
	Short: "Browse a peer's public files",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		peerAddr := args[0]

		conn, err := ipcDial(socketPath(browseNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if err := writeBrowsePeerRequest(conn, peerAddr); err != nil {
			return err
		}

		data, err := readJSONResponse(conn)
		if err != nil {
			return err
		}

		var files []State.PublicFileEntry
		if err := json.Unmarshal(data, &files); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		if len(files) == 0 {
			fmt.Printf("No public files on %s\n", peerAddr)
			return nil
		}

		fmt.Printf("Public files on %s:\n", peerAddr)
		fmt.Printf("%-40s %-8s %s\n", "KEY", "SIZE", "TYPE")
		for _, f := range files {
			typ := "file"
			if f.IsDir {
				typ = "dir"
			}
			fmt.Printf("%-40s %-8s %s\n", f.Key, humanSize(f.Size), typ)
		}
		return nil
	},
}

func init() {
	browseCmd.Flags().StringVarP(&browseNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(browseCmd)
}
