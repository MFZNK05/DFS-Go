package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var port string
var peers []string
var replicationFactor int
var noSTUN bool
var maxPeers int

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Hermod daemon server",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Starting daemon on", port)
		return StartDaemon(port, peers, replicationFactor, noSTUN, maxPeers)
	},
}

func init() {
	startCmd.Flags().StringVar(&port, "port", ":3000", "Listening port for this node")
	startCmd.Flags().StringSliceVar(&peers, "peer", []string{}, "Comma-separated list of peer addresses")
	startCmd.Flags().IntVar(&replicationFactor, "replication", 3, "Number of replicas per file (default 3)")
	startCmd.Flags().BoolVar(&noSTUN, "no-stun", false, "Disable STUN-based NAT traversal")
	startCmd.Flags().IntVar(&maxPeers, "max-peers", 8, "Target number of active peer connections")
	rootCmd.AddCommand(startCmd)
}
