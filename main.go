package main

import (
	// "bytes"
	// "fmt"
	// "io"
	// "log"
	// "time"

	//"time"

	//peer2peer "github.com/Faizan2005/DFS-Go/Peer2Peer"
	//server "github.com/Faizan2005/DFS-Go/Server"
	"github.com/Faizan2005/DFS-Go/cmd"
)

func main() {
	cmd.Execute()
}

// func main() {
// 	s1 := server.MakeServer(":3000", "")
// 	s2 := server.MakeServer(":7000", "")
// 	s3 := server.MakeServer(":5000", ":3000", ":7000")

// 	go func() { log.Fatal(s1.Run()) }()
// 	time.Sleep(500 * time.Millisecond)
// 	go func() { log.Fatal(s2.Run()) }()

// 	time.Sleep(2 * time.Second)

// 	go s3.Run()
// 	time.Sleep(2 * time.Second)

// 	for i := 0; i < 20; i++ {
// 		key := fmt.Sprintf("picture_%d.png", i)
// 		data := bytes.NewReader([]byte("my big data file here!"))
// 		s3.StoreData(key, data)

// 		// if err := s3.store.Delete(s3.ID, key); err != nil {
// 		// 	log.Fatal(err)
// 		// }

// 		r, err := s3.GetData(key)
// 		if err != nil {
// 			log.Fatal(err)
// 		}

// 		b, err := io.ReadAll(r)
// 		if err != nil {
// 			log.Fatal(err)
// 		}

// 		fmt.Println(string(b))
// 	}

// 	select {}
// }
