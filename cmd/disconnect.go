package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var disconnectNode string

var disconnectCmd = &cobra.Command{
	Use:   "disconnect <addr-or-alias>",
	Short: "Disconnect and blocklist a peer",
	Long:  "Disconnects from a peer and adds it to the local blocklist so it won't reconnect automatically. Use 'hermod unblock' to reverse.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := ipcDial(socketPath(disconnectNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if _, err := conn.Write([]byte{opcodeDisconnectPeer}); err != nil {
			return err
		}
		if err := writeString16(conn, args[0]); err != nil {
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
	disconnectCmd.Flags().StringVarP(&disconnectNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(disconnectCmd)
}
