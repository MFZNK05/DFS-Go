package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	uploadKey  string
	uploadFile string
)

var uploadCmd = &cobra.Command{
	Use:   "upload",
	Short: "Upload a file to the network",
	RunE: func(cmd *cobra.Command, args []string) error {
		if uploadKey == "" || uploadFile == "" {
			return fmt.Errorf("both --key and --file are required")
		}
		return UploadFile(uploadKey, uploadFile)
	},
}

func init() {
	uploadCmd.Flags().StringVarP(&uploadKey, "key", "k", "", "Key to associate the file with (required)")
	uploadCmd.Flags().StringVarP(&uploadFile, "file", "f", "", "Path to the file (required)")
	uploadCmd.MarkFlagRequired("key")
	uploadCmd.MarkFlagRequired("file")
	rootCmd.AddCommand(uploadCmd)
}
