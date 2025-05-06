package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var port string
var peers []string

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the DFS daemon server",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Starting daemon on", port)
		return StartDaemon(port, peers)
	},
}

func init() {
	startCmd.Flags().StringVar(&port, "port", ":3000", "Listening port for this node")
	startCmd.Flags().StringSliceVar(&peers, "peer", []string{}, "Comma-separated list of peer addresses")
	rootCmd.AddCommand(startCmd)
}
