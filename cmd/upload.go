package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	uploadKey  string
	uploadFile string
	uploadNode string
)

var uploadCmd = &cobra.Command{
	Use:   "upload",
	Short: "Upload a file to the network",
	RunE: func(cmd *cobra.Command, args []string) error {
		if uploadKey == "" || uploadFile == "" {
			return fmt.Errorf("both --key and --file are required")
		}
		return UploadFile(uploadKey, uploadFile, socketPath(uploadNode))
	},
}

func init() {
	uploadCmd.Flags().StringVarP(&uploadKey, "key", "k", "", "Key to associate the file with (required)")
	uploadCmd.Flags().StringVarP(&uploadFile, "file", "f", "", "Path to the file (required)")
	uploadCmd.Flags().StringVarP(&uploadNode, "node", "n", ":3000", "Port of the local node to upload through (e.g. :3000)")
	uploadCmd.MarkFlagRequired("key")
	uploadCmd.MarkFlagRequired("file")
	rootCmd.AddCommand(uploadCmd)
}
