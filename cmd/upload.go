package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	uploadKey  string
	uploadFile string
	//uploadPort string
)

var uploadCmd = &cobra.Command{
	Use:   "upload",
	Short: "Upload a file to the network",
	RunE: func(cmd *cobra.Command, args []string) error {
		if uploadKey == "" || uploadFile == "" {
			return fmt.Errorf("both --key and --file are required")
		}

		// f, err := os.Open(uploadFile)
		// if err != nil {
		// 	return fmt.Errorf("failed to open file: %v", err)
		// }
		// defer f.Close()

		// server := serve.MakeServer(uploadPort)
		// err = server.StoreData(uploadKey, f)
		// if err != nil {
		// 	return fmt.Errorf("failed to store data: %v", err)
		// }

		// fmt.Println("File uploaded successfully!")
		return UploadFile(uploadKey, uploadFile)
	},
}

func init() {
	// Add flags to the command (not just FlagSet)
	uploadCmd.Flags().StringVarP(&uploadKey, "key", "k", "", "Key to associate the file with (required)")
	uploadCmd.Flags().StringVarP(&uploadFile, "file", "f", "", "Path to the file (required)")
	//uploadCmd.Flags().StringVarP(&uploadPort, "port", "p", ":4000", "Port to run this peer on")

	// Mark required flags
	uploadCmd.MarkFlagRequired("key")
	uploadCmd.MarkFlagRequired("file")

	rootCmd.AddCommand(uploadCmd)
}
