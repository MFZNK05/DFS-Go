package cmd

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Faizan2005/DFS-Go/Crypto/identity"
	"github.com/Faizan2005/DFS-Go/tui"
	tea "github.com/charmbracelet/bubbletea"
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
		logDir := filepath.Join(homeDir, ".hermond")
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

	// 4. Redirect Go's standard log package to the daemon log file.
	// NOTE: Do NOT redirect os.Stdout/os.Stderr — bubbletea needs the
	// real stdout to render the TUI. The log.SetOutput here catches any
	// third-party library using log.Printf. The structured logger was
	// already redirected in StartDaemonAsync via logging.InitWithWriter.
	if logFile != nil {
		log.SetOutput(logFile)
	}

	// 5. Bubbletea debug log capture (catches tea's internal debug output).
	if homeDir != "" {
		teaLog, teaErr := tea.LogToFile(filepath.Join(homeDir, ".hermond", "tui-debug.log"), "debug")
		if teaErr == nil {
			defer teaLog.Close()
		}
	}

	// 6. Launch TUI (blocks until user quits).
	return tui.Run(handle.SockPath, tui.Handlers{
		UploadFile:      UploadFile,
		UploadDirectory: UploadDirectory,
		DownloadFile:    DownloadFile,
	})
}

// migrateConfigDir renames ~/.dfs to ~/.hermond for users upgrading from the old name.
func migrateConfigDir() {
	home, _ := os.UserHomeDir()
	if home == "" {
		return
	}
	oldDir := filepath.Join(home, ".dfs")
	newDir := filepath.Join(home, ".hermond")
	if _, err := os.Stat(newDir); err == nil {
		return // already migrated
	}
	if _, err := os.Stat(oldDir); err != nil {
		return // nothing to migrate
	}
	_ = os.Rename(oldDir, newDir)
}

func ensureIdentity() (*identity.Identity, error) {
	migrateConfigDir()
	path := identity.DefaultPath()
	if id, err := identity.Load(path); err == nil {
		return id, nil
	}

	fmt.Print("Welcome to Hermond! Enter your alias: ")
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
