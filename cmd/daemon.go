package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	serve "github.com/Faizan2005/DFS-Go/Server"
	"github.com/Faizan2005/DFS-Go/Observability/logging"
	"github.com/Faizan2005/DFS-Go/Observability/tracing"
	"github.com/Faizan2005/DFS-Go/factory"
)

func StartDaemon(port string, peers []string, replicationFactor int) error {
	// Sprint 5: structured logging (MakeServer also calls this; idempotent).
	logging.Init("daemon", logging.LevelInfo)

	// Sprint 5: tracing — disabled by default; set OTEL_EXPORTER_OTLP_ENDPOINT
	// (e.g. "http://localhost:4318") to export spans to Jaeger / any OTLP backend.
	otlpEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	shutdownTracing, err := tracing.Init("dfs-node", otlpEndpoint)
	if err != nil {
		logging.Global.Warn("tracing init failed, continuing without tracing", "err", err)
		shutdownTracing = func(_ context.Context) error { return nil }
	}

	socketPath := GetSocketPath()
	_ = os.RemoveAll(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()

	logging.Global.Info("transport protocol", "protocol", string(factory.ProtocolFromEnv()))
	server := serve.MakeServer(port, replicationFactor, peers...)

	// Graceful shutdown on SIGTERM / SIGINT.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logging.Global.Info("shutting down gracefully", "signal", sig.String())
		server.GracefulShutdown()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5e9) // 5 seconds
		defer shutdownCancel()
		_ = shutdownTracing(shutdownCtx)
		os.Exit(0)
	}()

	go server.Run()

	logging.Global.Info("daemon started", "socket", socketPath, "port", port)
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
