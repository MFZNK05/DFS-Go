//go:build linux

package cmd

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
)

func ensureFirewallRule(port int) error {
	flagPath, err := firewallFlagPath()
	if err != nil {
		return err
	}
	if firewallFlagCurrent(flagPath) {
		return nil // already configured with current version
	}

	ports := []int{port, port + 1, 5353} // control port + data port + mDNS

	// If running as root, apply directly.
	isRoot := os.Geteuid() == 0
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		isRoot = true
	}

	// Try ufw first (Ubuntu/Debian) — most common.
	if path, err := exec.LookPath("ufw"); err == nil {
		ok := true
		for _, p := range ports {
			portStr := fmt.Sprintf("%d/udp", p)
			var cmd *exec.Cmd
			if isRoot {
				cmd = exec.Command(path, "allow", portStr)
			} else {
				cmd = exec.Command("sudo", "-n", path, "allow", portStr)
			}
			if err := cmd.Run(); err != nil {
				ok = false
				break
			}
			log.Printf("[Firewall] ufw: allowed %s", portStr)
		}
		if ok {
			return writeFirewallFlag(flagPath)
		}
		// sudo -n failed (needs password) — fall through.
	}

	// Try firewall-cmd (Fedora/RHEL/CentOS).
	if path, err := exec.LookPath("firewall-cmd"); err == nil {
		ok := true
		for _, p := range ports {
			portStr := fmt.Sprintf("%d/udp", p)
			var cmd *exec.Cmd
			if isRoot {
				cmd = exec.Command(path, "--add-port="+portStr, "--permanent")
			} else {
				cmd = exec.Command("sudo", "-n", path, "--add-port="+portStr, "--permanent")
			}
			if err := cmd.Run(); err != nil {
				ok = false
				break
			}
			log.Printf("[Firewall] firewall-cmd: allowed %s", portStr)
		}
		if ok {
			// Reload to apply permanent rules.
			if isRoot {
				exec.Command(path, "--reload").Run()
			} else {
				exec.Command("sudo", "-n", path, "--reload").Run()
			}
			return writeFirewallFlag(flagPath)
		}
	}

	// Try raw iptables as last resort.
	if path, err := exec.LookPath("iptables"); err == nil {
		ok := true
		for _, p := range ports {
			args := []string{"-A", "INPUT", "-p", "udp", "--dport", fmt.Sprintf("%d", p), "-j", "ACCEPT"}
			var cmd *exec.Cmd
			if isRoot {
				cmd = exec.Command(path, args...)
			} else {
				cmd = exec.Command("sudo", append([]string{"-n", path}, args...)...)
			}
			if err := cmd.Run(); err != nil {
				ok = false
				break
			}
			log.Printf("[Firewall] iptables: allowed UDP %d", p)
		}
		if ok {
			return writeFirewallFlag(flagPath)
		}
	}

	// No known firewall tool found or no privilege — likely no firewall active.
	log.Printf("[Firewall] no firewall tool available or not root — skipping (may need: sudo ufw allow %d/udp)", port)
	return writeFirewallFlag(flagPath)
}
