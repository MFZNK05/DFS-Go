package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"log"

	"github.com/Faizan2005/DFS-Go/Crypto/identity"
	"github.com/Faizan2005/DFS-Go/Observability/logging"
	"github.com/Faizan2005/DFS-Go/Observability/memlog"
	"github.com/Faizan2005/DFS-Go/Observability/tracing"
	serve "github.com/Faizan2005/DFS-Go/Server"
	"github.com/Faizan2005/DFS-Go/factory"
)

// daemonStopFunc is the registered shutdown function for the running daemon.
// Set by StartDaemonAsync, called by handleShutdown (OpShutdown IPC).
var (
	daemonStopMu   sync.Mutex
	daemonStopFunc func()
)

func setDaemonStopFunc(fn func()) {
	daemonStopMu.Lock()
	daemonStopFunc = fn
	daemonStopMu.Unlock()
}

func getDaemonStopFunc() func() {
	daemonStopMu.Lock()
	defer daemonStopMu.Unlock()
	return daemonStopFunc
}

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
func StartDaemonAsync(port string, peers []string, replicationFactor int, disableSTUN bool, maxPeers int, logWriter ...io.Writer) (*DaemonHandle, error) {
	if len(logWriter) > 0 && logWriter[0] != nil {
		logging.InitWithWriter(logWriter[0], "daemon", logging.LevelInfo)
		log.SetOutput(logWriter[0])
	} else {
		logging.Init("daemon", logging.LevelInfo)
	}

	otlpEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	shutdownTracing, err := tracing.Init("hermod-node", otlpEndpoint)
	if err != nil {
		logging.Global.Warn("tracing init failed, continuing without tracing", "err", err)
		shutdownTracing = func(_ context.Context) error { return nil }
	}

	// Auto-configure OS firewall on first run (best-effort, non-fatal).
	portNum, _ := strconv.Atoi(strings.TrimLeft(port, ":"))
	if portNum == 0 {
		portNum = 3000
	}
	if err := ensureFirewallRule(portNum); err != nil {
		logging.Global.Warn("firewall auto-setup failed (manual config may be needed)", "err", err)
	}

	sockPath := socketPath(port)
	cleanupSocket(sockPath)

	listener, err := ipcListen(sockPath)
	if err != nil {
		return nil, err
	}

	// Root all runtime data (CAS storage, metadata DB) under ~/.hermod/
	// so the binary can be run from any working directory.
	homeDir, _ := os.UserHomeDir()
	dataDir := ""
	if homeDir != "" {
		dataDir = filepath.Join(homeDir, ".hermod")
		os.MkdirAll(dataDir, 0700)
	}

	makeOpts := &serve.MakeServerOpts{DisableSTUN: disableSTUN, MaxPeers: maxPeers, DataDir: dataDir}
	if v := os.Getenv("DFS_CACHE_LIMIT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			makeOpts.CacheLimit = n
		}
	}
	if id, err := identity.Load(identity.DefaultPath()); err == nil {
		logging.Global.Info("identity loaded", "alias", id.Alias, "fingerprint", id.Fingerprint())
		makeOpts.IdentityMeta = id.GossipMetadata()
		makeOpts.X25519Priv = id.X25519Priv
	} else {
		logging.Global.Warn("no identity found — ECDH sharing disabled. Run 'hermod identity init --alias <name>'")
	}

	if homeDir != "" {
		dbName := fmt.Sprintf("state-%s.db", strings.TrimLeft(port, ":"))
		makeOpts.StateDBPath = filepath.Join(dataDir, dbName)
	}

	// Two-Port Architecture: data transport on port+1 (automatic, no CLI flag needed).
	// Only enabled for QUIC (TCP doesn't benefit from port isolation).
	if factory.ProtocolFromEnv() == factory.ProtocolQUIC {
		dataPort := fmt.Sprintf(":%d", portNum+1)
		makeOpts.DataPort = dataPort
		logging.Global.Info("two-port mode", "control", port, "data", dataPort)
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

	// Start memory diagnostics monitor — logs heap stats every 30s and
	// auto-dumps heap profiles at 500MB/1GB/2GB/4GB thresholds so we have
	// forensic data even if the OOM killer sends SIGKILL.
	var memMon *memlog.Monitor
	if homeDir != "" {
		memMon = memlog.New(filepath.Join(homeDir, ".hermod"), 30*time.Second)
		memMon.Start()
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

	stopFn := func() {
		if memMon != nil {
			memMon.Stop()
		}
		server.GracefulShutdown()
		listener.Close()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5e9)
		defer shutdownCancel()
		_ = shutdownTracing(shutdownCtx)
	}

	// Register so handleShutdown (OpShutdown IPC) can call it.
	setDaemonStopFunc(stopFn)

	return &DaemonHandle{
		Server:   server,
		Listener: listener,
		SockPath: sockPath,
		Stop:     stopFn,
	}, nil
}

// StartDaemon starts the daemon and blocks until a signal is received.
func StartDaemon(port string, peers []string, replicationFactor int, disableSTUN bool, maxPeers int) error {
	handle, err := StartDaemonAsync(port, peers, replicationFactor, disableSTUN, maxPeers)
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
