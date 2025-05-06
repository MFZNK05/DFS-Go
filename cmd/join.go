package cmd

import (
	"fmt"
	"log"

	serve "github.com/Faizan2005/DFS-Go/Server"
	"github.com/spf13/cobra"
)

var joinPort string
var joinPeers []string

var joinCmd = &cobra.Command{
	Use:   "join",
	Short: "Join an existing network",
	RunE: func(cmd *cobra.Command, args []string) error {
		if joinPort == "" || len(joinPeers) == 0 {
			return fmt.Errorf("please specify --port and at least one --peer")
		}

		server := serve.MakeServer(joinPort, joinPeers...)

		go func() {
			if err := server.Run(); err != nil {
				log.Fatalf("Server error: %v", err)
			}
		}()

		select {} // keep the process running
	},
}

func init() {
	joinCmd.Flags().StringVar(&joinPort, "port", "", "Port to run this peer on")
	joinCmd.Flags().StringSliceVar(&joinPeers, "peer", nil, "Comma-separated list of bootstrap peers")
	rootCmd.AddCommand(joinCmd)
}
