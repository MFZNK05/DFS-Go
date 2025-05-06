package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "dfs",
	Short: "DFS is a distributed file system",
	Long:  `DFS is a distributed file system that allows peer-to-peer file storage and retrieval.`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
