package cmd

import (
	"log"
	"time"

	server "github.com/Faizan2005/DFS-Go/Server"
	"github.com/spf13/cobra"
)

var port string
var peers []string

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Start a DFS peer node",
	Run: func(cmd *cobra.Command, args []string) {
		s := server.MakeServer(port, peers...)
		log.Printf("Starting DFS node on %s...\n", port)
		time.Sleep(500 * time.Millisecond)
		log.Fatal(s.Run())
	},
}

func init() {
	initCmd.Flags().StringVarP(&port, "port", "p", ":3000", "Port to listen on")
	initCmd.Flags().StringSliceVarP(&peers, "peers", "b", []string{}, "Bootstrap peer addresses")
	rootCmd.AddCommand(initCmd)
}
