package cmd

import (
	"github.com/Faizan2005/DFS-Go/tui"
	"github.com/spf13/cobra"
)

var tuiNode string

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Launch the interactive TUI dashboard",
	RunE: func(cmd *cobra.Command, args []string) error {
		sock := SocketPath(tuiNode)
		return tui.Run(sock, tui.Handlers{
			UploadFile:      UploadFile,
			UploadDirectory: UploadDirectory,
			DownloadFile:    DownloadFile,
		})
	},
}

func init() {
	tuiCmd.Flags().StringVar(&tuiNode, "node", ":3000", "Node port to connect to")
	rootCmd.AddCommand(tuiCmd)
}
