package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	uploadName         string
	uploadFile         string
	uploadDir          string
	uploadNode         string
	uploadShareWith    string
	uploadShareWithKey string
	uploadPublic       bool
)

var uploadCmd = &cobra.Command{
	Use:   "upload",
	Short: "Upload a file or directory to the network",
	RunE: func(cmd *cobra.Command, args []string) error {
		if uploadName == "" {
			return fmt.Errorf("--name is required")
		}
		if uploadFile == "" && uploadDir == "" {
			return fmt.Errorf("either --file or --dir is required")
		}
		if uploadFile != "" && uploadDir != "" {
			return fmt.Errorf("--file and --dir are mutually exclusive")
		}
		if uploadDir != "" {
			return UploadDirectory(uploadName, uploadDir, uploadShareWith, uploadShareWithKey, socketPath(uploadNode), uploadPublic)
		}
		return UploadFile(uploadName, uploadFile, uploadShareWith, uploadShareWithKey, socketPath(uploadNode), uploadPublic)
	},
}

func init() {
	uploadCmd.Flags().StringVarP(&uploadName, "name", "k", "", "Name to associate the upload with (required)")
	uploadCmd.Flags().StringVarP(&uploadFile, "file", "f", "", "Path to a file to upload")
	uploadCmd.Flags().StringVarP(&uploadDir, "dir", "d", "", "Path to a directory to upload")
	uploadCmd.Flags().StringVarP(&uploadNode, "node", "n", ":3000", "Port of the local node to upload through (e.g. :3000)")
	uploadCmd.Flags().StringVar(&uploadShareWith, "share-with", "", "Comma-separated aliases to share with (ECDH encryption)")
	uploadCmd.Flags().StringVar(&uploadShareWithKey, "share-with-key", "", "Comma-separated X25519 pub keys to share with (fallback)")
	uploadCmd.Flags().BoolVar(&uploadPublic, "public", false, "Mark upload as publicly browsable by other peers")
	uploadCmd.MarkFlagRequired("name")
	rootCmd.AddCommand(uploadCmd)
}
