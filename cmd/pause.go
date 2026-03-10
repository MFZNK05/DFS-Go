package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var pauseNode string

var pauseCmd = &cobra.Command{
	Use:   "pause <transfer-id>",
	Short: "Pause an active transfer",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := ipcDial(socketPath(pauseNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if err := writePauseTransferRequest(conn, args[0]); err != nil {
			return err
		}

		ok, msg, err := readStatus(conn)
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		if !ok {
			return fmt.Errorf("%s", msg)
		}
		fmt.Println(msg)
		return nil
	},
}

func init() {
	pauseCmd.Flags().StringVarP(&pauseNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(pauseCmd)
}
