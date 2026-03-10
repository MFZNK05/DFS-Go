package cmd

import (
	"fmt"
	"net"

	"github.com/spf13/cobra"
)

var resumeNode string

var resumeCmd = &cobra.Command{
	Use:   "resume <transfer-id>",
	Short: "Resume a paused transfer",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := net.Dial("unix", socketPath(resumeNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if err := writeResumeTransferRequest(conn, args[0]); err != nil {
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
	resumeCmd.Flags().StringVarP(&resumeNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(resumeCmd)
}
