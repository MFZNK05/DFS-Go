package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "dfs",
	Short: "DFS – Distributed File System",
	Long:  `DFS is a distributed file system for peer-to-peer file storage and retrieval. Run without arguments to start the node and launch the TUI.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUnified()
	},
}

func init() {
	rootCmd.Flags().StringVar(&unifiedPort, "port", ":3000", "Listening port for the node")
	rootCmd.Flags().StringSliceVar(&unifiedPeers, "peer", nil, "Peer addresses to connect to")
	rootCmd.Flags().BoolVar(&unifiedNoSTUN, "no-stun", false, "Disable STUN NAT traversal")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
