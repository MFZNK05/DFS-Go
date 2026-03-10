package cmd

import (
	"fmt"

	"github.com/Faizan2005/DFS-Go/ipc"
	"github.com/spf13/cobra"
)

var sendNode string

var sendCmd = &cobra.Command{
	Use:   "send <file-key> <peer-addr-or-alias>",
	Short: "Send a file directly to a peer",
	Long:  "Sends a direct-share notification for one of your uploaded files to a specific peer. The file must already be uploaded (visible in 'hermond list uploads').",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		fileKey, peer := args[0], args[1]

		conn, err := ipcDial(socketPath(sendNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if _, err := conn.Write([]byte{ipc.OpSendToPeer}); err != nil {
			return err
		}
		if err := writeString16(conn, fileKey); err != nil {
			return err
		}
		if err := writeString16(conn, peer); err != nil {
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
	sendCmd.Flags().StringVarP(&sendNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(sendCmd)
}
