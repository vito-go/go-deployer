//go:build !windows

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"syscall"

	"github.com/vito-go/go-deployer/agent"
	"golang.org/x/sys/unix"
)

func main() {
	// Register with the control plane — non-blocking, runs in background
	agent.Register(agent.Config{
		ServerHost:  "localhost:2053",
		ServiceName: "user-api",
		Group:       "production",
		BinaryDir:   "user-api",
		AppArgs:     "",
		Port:        8080,
		Token:       "",
	})

	// Your normal application code
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello from user-api (PID: %d)", syscall.Getpid())
	})

	// SO_REUSEPORT listener for zero-downtime deployment
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			})
		},
	}
	ln, err := lc.Listen(context.Background(), "tcp", ":8080")
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("user-api starting on :8080 (PID: %d)", syscall.Getpid())
	log.Fatal(http.Serve(ln, mux))
}
