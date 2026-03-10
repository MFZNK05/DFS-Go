package cmd

import (
	"encoding/json"
	"fmt"
	"net"

	"github.com/spf13/cobra"
)

var peersNode string

var peersCmd = &cobra.Command{
	Use:   "peers",
	Short: "List connected peers",
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := net.Dial("unix", socketPath(peersNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if _, err := conn.Write([]byte{opcodeListPeers}); err != nil {
			return err
		}

		data, err := readJSONResponse(conn)
		if err != nil {
			return err
		}

		var peers []peerInfo
		if err := json.Unmarshal(data, &peers); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		if len(peers) == 0 {
			fmt.Println("No peers connected.")
			return nil
		}

		fmt.Printf("%-25s %-10s %-20s %-16s %s\n", "ADDR", "STATE", "ALIAS", "FINGERPRINT", "PUBLIC")
		for _, p := range peers {
			pub := ""
			if p.PublicCount != "" && p.PublicCount != "0" {
				pub = p.PublicCount + " files"
			}
			fp := p.Fingerprint
			if len(fp) > 16 {
				fp = fp[:16]
			}
			fmt.Printf("%-25s %-10s %-20s %-16s %s\n", p.Addr, p.State, p.Alias, fp, pub)
		}
		return nil
	},
}

func init() {
	peersCmd.Flags().StringVarP(&peersNode, "node", "n", ":3000", "Port of the local node")
	rootCmd.AddCommand(peersCmd)
}
