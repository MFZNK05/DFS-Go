package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	downloadKey  string
	outputFile   string
	downloadNode string
)

var downloadCmd = &cobra.Command{
	Use:   "download",
	Short: "Download a file from the network using a key",
	Long:  `Downloads a file from the distributed network using its unique key.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if downloadKey == "" {
			return fmt.Errorf("key is required")
		}
		return DownloadFile(downloadKey, outputFile, socketPath(downloadNode))
	},
}

func init() {
	downloadCmd.Flags().StringVarP(&downloadKey, "key", "k", "", "Key to retrieve the file (required)")
	downloadCmd.Flags().StringVarP(&outputFile, "output", "o", "", "Output file path (required)")
	downloadCmd.Flags().StringVarP(&downloadNode, "node", "n", ":3000", "Port of the local node to download through (e.g. :3001)")
	downloadCmd.MarkFlagRequired("key")
	rootCmd.AddCommand(downloadCmd)
}
