package cmd

import (
	"encoding/json"
	"fmt"

	server "github.com/Faizan2005/DFS-Go/Server"
	"github.com/spf13/cobra"
)

var searchNode string

var searchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search for files across the network",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := ipcDial(socketPath(searchNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if err := writeSearchRequest(conn, args[0]); err != nil {
			return err
		}

		data, err := readJSONResponse(conn)
		if err != nil {
			return err
		}

		var results []server.SearchResult
		if err := json.Unmarshal(data, &results); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		if len(results) == 0 {
			fmt.Println("No results found.")
			return nil
		}

		fmt.Printf("RESULTS (%d found):\n", len(results))
		fmt.Printf("%-40s %-25s %-8s %-15s %s\n", "KEY", "NAME", "SIZE", "OWNER", "NODE")
		for _, r := range results {
			name := r.Name
			if len(name) > 23 {
				name = name[:23] + ".."
			}
			key := r.Key
			if len(key) > 38 {
				key = key[:38] + ".."
			}
			owner := r.OwnerAlias
			if owner == "" {
				if len(r.OwnerFP) > 12 {
					owner = r.OwnerFP[:12] + ".."
				} else {
					owner = r.OwnerFP
				}
			}
			fmt.Printf("%-40s %-25s %-8s %-15s %s\n", key, name, humanSize(r.Size), owner, r.NodeAddr)
		}
		return nil
	},
}

func init() {
	searchCmd.Flags().StringVarP(&searchNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(searchCmd)
}
