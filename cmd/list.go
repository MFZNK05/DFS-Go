package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/Faizan2005/DFS-Go/State"
	"github.com/spf13/cobra"
)

var (
	listNode           string
	listUploadsPublic  bool
	listUploadsPrivate bool
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List uploads or downloads",
}

var listUploadsCmd = &cobra.Command{
	Use:   "uploads",
	Short: "List upload history",
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := net.Dial("unix", socketPath(listNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if _, err := conn.Write([]byte{opcodeListUploads}); err != nil {
			return err
		}

		data, err := readJSONResponse(conn)
		if err != nil {
			return err
		}

		var uploads []State.UploadEntry
		if err := json.Unmarshal(data, &uploads); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		// Apply client-side filter.
		if listUploadsPublic {
			var filtered []State.UploadEntry
			for _, u := range uploads {
				if u.Public {
					filtered = append(filtered, u)
				}
			}
			uploads = filtered
		} else if listUploadsPrivate {
			var filtered []State.UploadEntry
			for _, u := range uploads {
				if u.Encrypted {
					filtered = append(filtered, u)
				}
			}
			uploads = filtered
		}

		if len(uploads) == 0 {
			fmt.Println("No uploads recorded.")
			return nil
		}

		fmt.Printf("%-30s %-8s %-8s %-6s %s\n", "NAME", "SIZE", "TYPE", "PUBLIC", "UPLOADED")
		for _, u := range uploads {
			t := time.Unix(0, u.Timestamp).Format("2006-01-02 15:04")
			typ := "file"
			if u.IsDir {
				typ = "dir"
			}
			pub := ""
			if u.Public {
				pub = "yes"
			}
			fmt.Printf("%-30s %-8s %-8s %-6s %s\n", u.Name, humanSize(u.Size), typ, pub, t)
		}
		return nil
	},
}

// listPublicCmd lists the public file catalog (what peers see when browsing).
var listPublicCmd = &cobra.Command{
	Use:   "public",
	Short: "List public file catalog",
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := net.Dial("unix", socketPath(listNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if _, err := conn.Write([]byte{opcodeListUploads}); err != nil {
			return err
		}

		data, err := readJSONResponse(conn)
		if err != nil {
			return err
		}

		var uploads []State.UploadEntry
		if err := json.Unmarshal(data, &uploads); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		// Filter to public only.
		var pub []State.UploadEntry
		for _, u := range uploads {
			if u.Public {
				pub = append(pub, u)
			}
		}

		if len(pub) == 0 {
			fmt.Println("No public files.")
			return nil
		}

		fmt.Printf("%-30s %-8s %-8s %s\n", "NAME", "SIZE", "TYPE", "UPLOADED")
		for _, u := range pub {
			t := time.Unix(0, u.Timestamp).Format("2006-01-02 15:04")
			typ := "file"
			if u.IsDir {
				typ = "dir"
			}
			fmt.Printf("%-30s %-8s %-8s %s\n", u.Name, humanSize(u.Size), typ, t)
		}
		return nil
	},
}

var listDownloadsCmd = &cobra.Command{
	Use:   "downloads",
	Short: "List download history",
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := net.Dial("unix", socketPath(listNode))
		if err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		defer conn.Close()

		if _, err := conn.Write([]byte{opcodeListDownloads}); err != nil {
			return err
		}

		data, err := readJSONResponse(conn)
		if err != nil {
			return err
		}

		var downloads []State.DownloadEntry
		if err := json.Unmarshal(data, &downloads); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		if len(downloads) == 0 {
			fmt.Println("No downloads recorded.")
			return nil
		}

		fmt.Printf("%-30s %-8s %-8s %s\n", "NAME", "SIZE", "TYPE", "DOWNLOADED")
		for _, d := range downloads {
			t := time.Unix(0, d.Timestamp).Format("2006-01-02 15:04")
			typ := "file"
			if d.IsDir {
				typ = "dir"
			}
			fmt.Printf("%-30s %-8s %-8s %s\n", d.Name, humanSize(d.Size), typ, t)
		}
		return nil
	},
}

func humanSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func init() {
	listCmd.PersistentFlags().StringVarP(&listNode, "node", "n", ":3000", "Port of the local node")
	listUploadsCmd.Flags().BoolVar(&listUploadsPublic, "public", false, "Show only public uploads")
	listUploadsCmd.Flags().BoolVar(&listUploadsPrivate, "private", false, "Show only encrypted/shared uploads")
	listCmd.AddCommand(listUploadsCmd)
	listCmd.AddCommand(listDownloadsCmd)
	listCmd.AddCommand(listPublicCmd)
	rootCmd.AddCommand(listCmd)
}
