package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	downloadKey string
	//	downloadPort string
	outputFile string // New flag for output file
)

var downloadCmd = &cobra.Command{
	Use:   "download",
	Short: "Download a file from the network using a key",
	Long:  `Downloads a file from the distributed network using its unique key.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if downloadKey == "" {
			return fmt.Errorf("key is required")
		}

		return DownloadFile(downloadKey, outputFile)
	},
}

func init() {
	// Add flags with shorthand versions
	downloadCmd.Flags().StringVarP(&downloadKey, "key", "k", "", "Key to retrieve the file (required)")
	//downloadCmd.Flags().StringVarP(&downloadPort, "port", "p", ":4000", "Port to run this peer on")
	downloadCmd.Flags().StringVarP(&outputFile, "output", "o", "", "Output file (default prints to stdout)")

	// Mark required flags
	downloadCmd.MarkFlagRequired("key")

	rootCmd.AddCommand(downloadCmd)
}
