package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
)

func DownloadFile(key, outputPath string) error {
	conn, err := net.Dial("unix", GetSocketPath())
	if err != nil {
		return err
	}
	defer conn.Close()

	req := map[string]string{
		"action": "download",
		"key":    key,
	}
	b, _ := json.Marshal(req)
	_, err = conn.Write(b)
	if err != nil {
		return err
	}

	resp, err := io.ReadAll(conn)
	if err != nil {
		return err
	}

	err = os.WriteFile(outputPath, resp, 0644)
	if err != nil {
		return err
	}

	fmt.Println("Downloaded to:", outputPath)
	return nil
}
