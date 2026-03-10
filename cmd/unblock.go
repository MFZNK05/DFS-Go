package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var unblockNode string

var unblockCmd = &cobra.Command{
	Use:   "unblock <addr-or-alias>",
	Short: "Remove a peer from the blocklist",
	Long:  "Removes a previously disconnected peer from the blocklist, allowing it to reconnect. Use 'hermond connect' afterwards to re-establish the connection.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := ipcDial(socketPath(unblockNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if _, err := conn.Write([]byte{opcodeUnblockPeer}); err != nil {
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
	unblockCmd.Flags().StringVarP(&unblockNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(unblockCmd)
}
