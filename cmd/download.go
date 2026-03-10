package cmd

import (
	"fmt"

	"github.com/Faizan2005/DFS-Go/Crypto/identity"
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
	Short: "Download a file or directory from the network using a name",
	Long:  `Downloads a file or directory from the distributed network using its unique name. Directory detection is automatic.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if downloadName == "" {
			return fmt.Errorf("name is required")
		}
		sock := socketPath(downloadNode)

		// Unified stat: one IPC round-trip tells us if it's a file or directory.
		id, err := identity.Load(identity.DefaultPath())
		if err != nil {
			return fmt.Errorf("no identity found. Run 'dfs identity init --alias <name>' first")
		}
		ownerFingerprint, err := resolveOwner(id, downloadFrom, downloadFromKey, sock)
		if err != nil {
			return err
		}
		storageKey := ownerFingerprint + "/" + downloadName
		resp, err := fetchManifestInfo(storageKey, sock)
		if err != nil {
			return err
		}

		if resp.IsDirectory {
			if outputFile == "" {
				return fmt.Errorf("--output is required for directory downloads (target directory path)")
			}
			return DownloadDirectory(downloadName, outputFile, downloadFrom, downloadFromKey, sock)
		}
		return DownloadFile(downloadName, outputFile, downloadFrom, downloadFromKey, sock)
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
