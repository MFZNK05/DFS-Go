package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Faizan2005/DFS-Go/Crypto/identity"
	"github.com/Faizan2005/DFS-Go/tui"
)

var (
	unifiedPort  string
	unifiedPeers []string
	unifiedNoSTUN bool
)

func runUnified() error {
	// 1. Ensure identity exists (prompt for alias if missing).
	if _, err := ensureIdentity(); err != nil {
		return err
	}

	// 2. Open log file for daemon output so it doesn't corrupt the TUI.
	homeDir, _ := os.UserHomeDir()
	var logFile *os.File
	if homeDir != "" {
		logDir := filepath.Join(homeDir, ".dfs")
		os.MkdirAll(logDir, 0700)
		var err error
		logFile, err = os.OpenFile(filepath.Join(logDir, "daemon.log"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err == nil {
			defer logFile.Close()
		} else {
			logFile = nil
		}
	}

	// 3. Start daemon in background (pass log file so all output goes there).
	handle, err := StartDaemonAsync(unifiedPort, unifiedPeers, 3, unifiedNoSTUN, 0, 8, logFile)
	if err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	defer handle.Stop()

	// 4. Launch TUI (blocks until user quits).
	return tui.Run(handle.SockPath, tui.Handlers{
		UploadFile:      UploadFile,
		UploadDirectory: UploadDirectory,
		DownloadFile:    DownloadFile,
	})
}

func ensureIdentity() (*identity.Identity, error) {
	path := identity.DefaultPath()
	if id, err := identity.Load(path); err == nil {
		return id, nil
	}

	fmt.Print("Welcome to DFS! Enter your alias: ")
	var alias string
	fmt.Scanln(&alias)
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil, fmt.Errorf("alias cannot be empty")
	}

	id, err := identity.Generate(alias)
	if err != nil {
		return nil, fmt.Errorf("generate identity: %w", err)
	}
	if err := id.Save(path); err != nil {
		return nil, fmt.Errorf("save identity: %w", err)
	}

	fmt.Printf("Identity created: %s (fingerprint: %s)\n", id.Alias, id.Fingerprint())
	return id, nil
}
