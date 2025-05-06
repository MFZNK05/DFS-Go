package cmd

import (
	"fmt"
	"log"
	"net"
	"os"

	serve "github.com/Faizan2005/DFS-Go/Server"
	//cmd "github.com/Faizan2005/DFS-Go/cmd"
	//internal "github.com/Faizan2005/DFS-Go/internal"
)

func StartDaemon(port string, peers []string) error {
	socketPath := GetSocketPath()

	_ = os.RemoveAll(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}

	defer listener.Close()

	server := serve.MakeServer(port, peers...)
	go server.Run()

	fmt.Println("Daemon started at", socketPath)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println("Connection error:", err)
			continue
		}

		go HandleClient(conn, server)
	}
}
