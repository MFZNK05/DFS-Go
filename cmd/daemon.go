package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/Faizan2005/DFS-Go/Crypto/identity"
	"github.com/Faizan2005/DFS-Go/Observability/logging"
	"github.com/Faizan2005/DFS-Go/Observability/tracing"
	serve "github.com/Faizan2005/DFS-Go/Server"
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

	sockPath := socketPath(port)
	_ = os.RemoveAll(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	defer listener.Close()

	// Load identity for gossip metadata (optional — warn if missing).
	var makeOpts *serve.MakeServerOpts
	if id, err := identity.Load(identity.DefaultPath()); err == nil {
		logging.Global.Info("identity loaded", "alias", id.Alias, "fingerprint", id.Fingerprint())
		makeOpts = &serve.MakeServerOpts{IdentityMeta: id.GossipMetadata()}
	} else {
		logging.Global.Warn("no identity found — ECDH sharing disabled. Run 'dfs identity init --alias <name>'")
	}

	logging.Global.Info("transport protocol", "protocol", string(factory.ProtocolFromEnv()))
	server := serve.MakeServer(port, replicationFactor, makeOpts, peers...)

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

	if err := server.Start(); err != nil {
		return fmt.Errorf("server failed to start: %w", err)
	}

	logging.Global.Info("daemon started", "socket", sockPath, "port", port)
	fmt.Println("Daemon started at", sockPath)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println("Connection error:", err)
			continue
		}
		go HandleClient(conn, server)
	}
}
