//go:build darwin

package cmd

import (
	"fmt"
	"os"
	"os/exec"
)

func ensureFirewallRule(port int) error {
	flagPath, err := firewallFlagPath()
	if err != nil {
		return err
	}
	if firewallFlagCurrent(flagPath) {
		return nil // already configured with current version
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	// macOS Application Firewall: add and unblock the binary.
	// osascript prompts the user for admin credentials.
	sfw := "/usr/libexec/ApplicationFirewall/socketfilterfw"
	script := fmt.Sprintf(
		`do shell script "%s --add %s && %s --unblockapp %s" with administrator privileges`,
		sfw, exePath, sfw, exePath,
	)
	cmd := exec.Command("osascript", "-e", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("macOS firewall setup failed: %w", err)
	}
	return writeFirewallFlag(flagPath)
}
