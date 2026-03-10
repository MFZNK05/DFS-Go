package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"log"

	"github.com/Faizan2005/DFS-Go/Crypto/identity"
	"github.com/Faizan2005/DFS-Go/Observability/logging"
	"github.com/Faizan2005/DFS-Go/Observability/tracing"
	serve "github.com/Faizan2005/DFS-Go/Server"
	"github.com/Faizan2005/DFS-Go/factory"
)

// DaemonHandle holds references to a running daemon for shutdown.
type DaemonHandle struct {
	Server   *serve.Server
	Listener net.Listener
	SockPath string
	Stop     func()
}

// StartDaemonAsync starts the daemon without blocking. Returns a handle
// for the caller to stop the daemon or retrieve the socket path.
// When logWriter is non-nil, all daemon logs are written there instead of stdout
// (used by the TUI to prevent log output from corrupting the terminal).
func StartDaemonAsync(port string, peers []string, replicationFactor int, disableSTUN bool, maxTransfers, maxPeers int, logWriter ...io.Writer) (*DaemonHandle, error) {
	if len(logWriter) > 0 && logWriter[0] != nil {
		logging.InitWithWriter(logWriter[0], "daemon", logging.LevelInfo)
		log.SetOutput(logWriter[0])
	} else {
		logging.Init("daemon", logging.LevelInfo)
	}

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
		return nil, err
	}

	makeOpts := &serve.MakeServerOpts{DisableSTUN: disableSTUN, MaxTransfers: maxTransfers, MaxPeers: maxPeers}
	if id, err := identity.Load(identity.DefaultPath()); err == nil {
		logging.Global.Info("identity loaded", "alias", id.Alias, "fingerprint", id.Fingerprint())
		makeOpts.IdentityMeta = id.GossipMetadata()
	} else {
		logging.Global.Warn("no identity found — ECDH sharing disabled. Run 'dfs identity init --alias <name>'")
	}

	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		stateDir := filepath.Join(homeDir, ".dfs")
		os.MkdirAll(stateDir, 0700)
		dbName := fmt.Sprintf("state-%s.db", strings.TrimLeft(port, ":"))
		makeOpts.StateDBPath = filepath.Join(stateDir, dbName)
	}

	logging.Global.Info("transport protocol", "protocol", string(factory.ProtocolFromEnv()))
	server := serve.MakeServer(port, replicationFactor, makeOpts, peers...)

	if err := server.Start(); err != nil {
		listener.Close()
		return nil, fmt.Errorf("server failed to start: %w", err)
	}

	// Backfill any old download/transfer records that have Size=0 (pre-SetSize fix).
	if server.StateDB != nil {
		fixed, migErr := server.StateDB.MigrateZeroSizes(func(key string, isDir bool) int64 {
			if isDir {
				if dm, err := server.GetDirectoryManifest(key); err == nil {
					return dm.TotalSize
				}
			}
			if sz := server.ContentLength(key); sz > 0 {
				return sz
			}
			// Manifest may not be cached locally yet. Try fetching it.
			if m, err := server.EnsureManifest(key); err == nil && m != nil {
				return m.TotalSize
			}
			return 0
		})
		if migErr != nil {
			log.Printf("state: size migration error: %v", migErr)
		} else if fixed > 0 {
			log.Printf("state: migrated %d zero-size records", fixed)
		}
	}

	logging.Global.Info("daemon started", "socket", sockPath, "port", port)

	// Accept loop in background goroutine.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // listener closed = shutdown
			}
			go HandleClient(conn, server)
		}
	}()

	return &DaemonHandle{
		Server:   server,
		Listener: listener,
		SockPath: sockPath,
		Stop: func() {
			server.GracefulShutdown()
			listener.Close()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5e9)
			defer shutdownCancel()
			_ = shutdownTracing(shutdownCtx)
		},
	}, nil
}

// StartDaemon starts the daemon and blocks until a signal is received.
func StartDaemon(port string, peers []string, replicationFactor int, disableSTUN bool, maxTransfers, maxPeers int) error {
	handle, err := StartDaemonAsync(port, peers, replicationFactor, disableSTUN, maxTransfers, maxPeers)
	if err != nil {
		return err
	}

	fmt.Println("Daemon started at", handle.SockPath)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	logging.Global.Info("shutting down gracefully", "signal", sig.String())
	handle.Stop()
	return nil
}
