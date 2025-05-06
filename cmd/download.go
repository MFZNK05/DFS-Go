package cmd

import (
	"fmt"
	"io"
	"os"

	serve "github.com/Faizan2005/DFS-Go/Server"
	"github.com/spf13/cobra"
)

var (
	downloadKey  string
	downloadPort string
	outputFile   string // New flag for output file
)

var downloadCmd = &cobra.Command{
	Use:   "download",
	Short: "Download a file from the network using a key",
	Long:  `Downloads a file from the distributed network using its unique key.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if downloadKey == "" {
			return fmt.Errorf("key is required")
		}

		server := serve.MakeServer(downloadPort)
		r, err := server.GetData(downloadKey)
		if err != nil {
			return fmt.Errorf("download failed: %v", err)
		}

		// Either output to file or stdout
		if outputFile != "" {
			file, err := os.Create(outputFile)
			if err != nil {
				return fmt.Errorf("could not create output file: %v", err)
			}
			defer file.Close()

			_, err = io.Copy(file, r)
			if err != nil {
				return fmt.Errorf("write failed: %v", err)
			}
			fmt.Printf("File saved to: %s\n", outputFile)
		} else {
			data, err := io.ReadAll(r)
			if err != nil {
				return fmt.Errorf("read failed: %v", err)
			}
			fmt.Printf("Downloaded content:\n%s\n", string(data))
		}
		return nil
	},
}

func init() {
	// Add flags with shorthand versions
	downloadCmd.Flags().StringVarP(&downloadKey, "key", "k", "", "Key to retrieve the file (required)")
	downloadCmd.Flags().StringVarP(&downloadPort, "port", "p", ":4000", "Port to run this peer on")
	downloadCmd.Flags().StringVarP(&outputFile, "output", "o", "", "Output file (default prints to stdout)")

	// Mark required flags
	downloadCmd.MarkFlagRequired("key")

	rootCmd.AddCommand(downloadCmd)
}
