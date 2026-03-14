package cmd

import (
	"os"
	"path/filepath"
)

const firewallFlagFile = ".firewall-configured"
const firewallFlagVersion = "v4" // bump to re-trigger rule creation (drop program= filter on Windows)

func firewallFlagPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hermod", firewallFlagFile), nil
}

// firewallFlagCurrent returns true if the flag file exists AND contains
// the current version stamp. Older stamps (or missing files) return false
// so that new rules (e.g. mDNS) get created on upgrade.
func firewallFlagCurrent(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return string(data) == firewallFlagVersion+"\n"
}

func writeFirewallFlag(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(firewallFlagVersion+"\n"), 0600)
}
