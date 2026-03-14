package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Faizan2005/DFS-Go/Crypto/identity"
	"github.com/Faizan2005/DFS-Go/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

var (
	unifiedPort   string
	unifiedPeers  []string
	unifiedNoSTUN bool
)

func runUnified(cmd *cobra.Command) error {
	// 1. Ensure identity exists (prompt for alias + port if missing).
	if _, err := ensureIdentity(); err != nil {
		return err
	}

	// 2. If --port wasn't explicitly set, use the saved config port.
	if !cmd.Flags().Changed("port") {
		if cfg := loadConfig(); cfg != nil && cfg.Port > 0 {
			unifiedPort = fmt.Sprintf(":%d", cfg.Port)
		}
	}

	sockPath := socketPath(unifiedPort)

	// 2. Check if a daemon is already running by dialing the socket.
	daemonAlreadyRunning := isDaemonRunning(sockPath)

	// 3. If no daemon, fork one as a detached background process.
	if !daemonAlreadyRunning {
		if err := forkDaemon(); err != nil {
			return fmt.Errorf("start daemon: %w", err)
		}
		// Wait for daemon to become ready (socket becomes connectable).
		if err := waitForDaemon(sockPath, 10*time.Second); err != nil {
			return fmt.Errorf("daemon did not start: %w", err)
		}
	}

	// 4. Bubbletea debug log capture.
	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		os.MkdirAll(filepath.Join(homeDir, ".hermod"), 0700)
		teaLog, teaErr := tea.LogToFile(filepath.Join(homeDir, ".hermod", "tui-debug.log"), "debug")
		if teaErr == nil {
			defer teaLog.Close()
		}
	}

	// 5. Launch TUI as a stateless IPC client (blocks until user quits).
	err := tui.Run(sockPath, tui.Handlers{
		UploadFile:      UploadFile,
		UploadDirectory: UploadDirectory,
		DownloadFile:    DownloadFile,
	})

	// 6. TUI exited — print background reminder.
	fmt.Println("\nHermod is still running in the background. Use 'hermod stop' to shut it down.")
	return err
}

// isDaemonRunning checks if a daemon is listening on the given socket path
// by attempting a real connection with a short timeout.
func isDaemonRunning(sockPath string) bool {
	conn, err := ipcDialTimeout(sockPath, 500*time.Millisecond)
	if err != nil {
		// Socket might be stale — clean it up.
		cleanupSocket(sockPath)
		return false
	}
	conn.Close()
	return true
}

// forkDaemon spawns `hermod start` as a fully detached background process.
// Stdout/stderr are redirected to the daemon log file.
func forkDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	args := []string{"start", "--port", unifiedPort}
	for _, p := range unifiedPeers {
		args = append(args, "--peer", p)
	}
	if unifiedNoSTUN {
		args = append(args, "--no-stun")
	}

	cmd := exec.Command(exe, args...)

	// Redirect child stdout/stderr to daemon log file.
	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		logDir := filepath.Join(homeDir, ".hermod")
		os.MkdirAll(logDir, 0700)
		logFile, err := os.OpenFile(filepath.Join(logDir, "daemon.log"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err == nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
			// logFile is inherited by the child — we can close our handle
			// after Start() returns.
			defer logFile.Close()
		}
	} else {
		cmd.Stdout = nil
		cmd.Stderr = nil
	}

	// Detach: new session (Unix) or new process group (Windows).
	detachCmd(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("fork daemon: %w", err)
	}

	// Release the child process so it survives our exit.
	cmd.Process.Release()
	return nil
}

// waitForDaemon polls the socket until the daemon is connectable or timeout.
func waitForDaemon(sockPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := ipcDialTimeout(sockPath, 300*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for daemon socket %s", sockPath)
}

// migrateConfigDir renames ~/.dfs to ~/.hermod for users upgrading from the old name.
func migrateConfigDir() {
	home, _ := os.UserHomeDir()
	if home == "" {
		return
	}
	oldDir := filepath.Join(home, ".dfs")
	newDir := filepath.Join(home, ".hermod")
	if _, err := os.Stat(newDir); err == nil {
		return // already migrated
	}
	if _, err := os.Stat(oldDir); err != nil {
		return // nothing to migrate
	}
	_ = os.Rename(oldDir, newDir)
}

// hermodConfig is the persistent first-run configuration stored at ~/.hermod/config.json.
type hermodConfig struct {
	Port int `json:"port"`
}

func configPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".hermod", "config.json")
}

func loadConfig() *hermodConfig {
	p := configPath()
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var cfg hermodConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	return &cfg
}

func saveConfig(cfg *hermodConfig) error {
	p := configPath()
	if p == "" {
		return nil
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0644)
}

func ensureIdentity() (*identity.Identity, error) {
	migrateConfigDir()
	path := identity.DefaultPath()
	if id, err := identity.Load(path); err == nil {
		return id, nil
	}

	// First-time setup — prompt for alias and port.
	fmt.Println("Welcome to Hermod!")
	fmt.Print("Enter your alias: ")
	var alias string
	fmt.Scanln(&alias)
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil, fmt.Errorf("alias cannot be empty")
	}

	// Prompt for control port (data port = control + 1, automatic).
	fmt.Print("Control port (default 3000, data port will be 3001): ")
	var portInput string
	fmt.Scanln(&portInput)
	portInput = strings.TrimSpace(portInput)
	chosenPort := 3000
	if portInput != "" {
		p, err := strconv.Atoi(portInput)
		if err != nil || p < 1 || p > 65534 {
			return nil, fmt.Errorf("invalid port: %s (must be 1-65534)", portInput)
		}
		chosenPort = p
	}

	id, err := identity.Generate(alias)
	if err != nil {
		return nil, fmt.Errorf("generate identity: %w", err)
	}
	if err := id.Save(path); err != nil {
		return nil, fmt.Errorf("save identity: %w", err)
	}

	// Save port preference.
	if err := saveConfig(&hermodConfig{Port: chosenPort}); err != nil {
		fmt.Printf("Warning: could not save port config: %v\n", err)
	}

	fmt.Printf("Identity created: %s (fingerprint: %s)\n", id.Alias, id.Fingerprint())
	fmt.Printf("Port configured: %d (data port: %d)\n", chosenPort, chosenPort+1)
	return id, nil
}
