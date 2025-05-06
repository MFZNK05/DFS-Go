package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	//internal "github.com/Faizan2005/DFS-Go/internal"
)

func UploadFile(key, filePath string) error {
	conn, err := net.Dial("unix", GetSocketPath())
	if err != nil {
		return err
	}

	defer conn.Close()

	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	req := map[string]string{
		"action": "upload",
		"key":    key,
		"data":   string(data),
	}

	b, err := json.Marshal(req)
	if err != nil {
		return err
	}

	_, err = conn.Write(b)
	if err != nil {
		return err
	}

	resp := make([]byte, 1024)
	n, err := conn.Read(resp)
	if err != nil {
		return err
	}

	fmt.Println("Server response:", string(resp[:n]))
	return nil

}
