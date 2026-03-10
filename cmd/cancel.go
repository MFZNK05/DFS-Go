package cmd

import (
	"fmt"
	"net"

	"github.com/spf13/cobra"
)

var cancelNode string

var cancelCmd = &cobra.Command{
	Use:   "cancel <transfer-id>",
	Short: "Cancel an active transfer",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := net.Dial("unix", socketPath(cancelNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if err := writeCancelTransferRequest(conn, args[0]); err != nil {
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
	cancelCmd.Flags().StringVarP(&cancelNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(cancelCmd)
}
