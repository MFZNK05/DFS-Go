package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	downloadName    string
	outputFile      string
	downloadNode    string
	downloadFrom    string
	downloadFromKey string
)

var downloadCmd = &cobra.Command{
	Use:   "download",
	Short: "Download a file from the network using a name",
	Long:  `Downloads a file from the distributed network using its unique name.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if downloadName == "" {
			return fmt.Errorf("name is required")
		}
		return DownloadFile(downloadName, outputFile, downloadFrom, downloadFromKey, socketPath(downloadNode))
	},
}

func init() {
	downloadCmd.Flags().StringVarP(&downloadName, "name", "k", "", "Name to retrieve the file (required)")
	downloadCmd.Flags().StringVarP(&outputFile, "output", "o", "", "Output file path (optional, defaults to stdout)")
	downloadCmd.Flags().StringVarP(&downloadNode, "node", "n", ":3000", "Port of the local node to download through (e.g. :3001)")
	downloadCmd.Flags().StringVar(&downloadFrom, "from", "", "Alias of the file owner (defaults to self)")
	downloadCmd.Flags().StringVar(&downloadFromKey, "from-key", "", "Fingerprint of the file owner (fallback for duplicate aliases)")
	downloadCmd.MarkFlagRequired("name")
	rootCmd.AddCommand(downloadCmd)
}
