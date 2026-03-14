package cmd

import (
	"fmt"

	"github.com/Faizan2005/DFS-Go/Crypto/identity"
	"github.com/spf13/cobra"
)

var removeNode string

var removeCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a file from the network (with tombstone propagation)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := identity.Load(identity.DefaultPath())
		if err != nil {
			return fmt.Errorf("no identity found. Run 'hermod identity init --alias <name>' first")
		}

		key := id.Fingerprint() + "/" + args[0]

		conn, err := ipcDial(socketPath(removeNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if err := writeRemoveFileRequest(conn, key); err != nil {
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
	removeCmd.Flags().StringVarP(&removeNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(removeCmd)
}
