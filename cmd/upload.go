package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	uploadName         string
	uploadFile         string
	uploadNode         string
	uploadShareWith    string
	uploadShareWithKey string
)

var uploadCmd = &cobra.Command{
	Use:   "upload",
	Short: "Upload a file to the network",
	RunE: func(cmd *cobra.Command, args []string) error {
		if uploadName == "" || uploadFile == "" {
			return fmt.Errorf("both --name and --file are required")
		}
		return UploadFile(uploadName, uploadFile, uploadShareWith, uploadShareWithKey, socketPath(uploadNode))
	},
}

func init() {
	uploadCmd.Flags().StringVarP(&uploadName, "name", "k", "", "Name to associate the file with (required)")
	uploadCmd.Flags().StringVarP(&uploadFile, "file", "f", "", "Path to the file (required)")
	uploadCmd.Flags().StringVarP(&uploadNode, "node", "n", ":3000", "Port of the local node to upload through (e.g. :3000)")
	uploadCmd.Flags().StringVar(&uploadShareWith, "share-with", "", "Comma-separated aliases to share with (ECDH encryption)")
	uploadCmd.Flags().StringVar(&uploadShareWithKey, "share-with-key", "", "Comma-separated X25519 pub keys to share with (fallback)")
	uploadCmd.MarkFlagRequired("name")
	uploadCmd.MarkFlagRequired("file")
	rootCmd.AddCommand(uploadCmd)
}
