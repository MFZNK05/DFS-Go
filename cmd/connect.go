package cmd

import (
	"fmt"
	"net"

	"github.com/spf13/cobra"
)

var connectNode string

var connectCmd = &cobra.Command{
	Use:   "connect <addr-or-alias>",
	Short: "Connect to a peer by address or alias",
	Long:  "Manually connect to a peer. If the peer was previously blocklisted, the blocklist entry is removed.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := net.Dial("unix", socketPath(connectNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if _, err := conn.Write([]byte{opcodeConnectPeer}); err != nil {
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
	connectCmd.Flags().StringVarP(&connectNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(connectCmd)
}
