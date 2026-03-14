//go:build windows

package cmd

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

func ensureFirewallRule(port int) error {
	flagPath, err := firewallFlagPath()
	if err != nil {
		return err
	}
	if firewallFlagCurrent(flagPath) {
		return nil // already configured with current version
	}

	// Control port rule (port-only, no program= filter).
	if err := ensureOneRule("Hermod", port); err != nil {
		return err
	}

	// Data port rule (port+1) — non-fatal on failure (falls back to single-port).
	if err := ensureOneRule("Hermod Data", port+1); err != nil {
		log.Printf("[Firewall] data port rule (UDP %d) failed — two-port mode may not work: %v", port+1, err)
	}

	// mDNS rule (UDP 5353) — non-fatal on failure.
	if err := ensureOneRule("Hermod mDNS", 5353); err != nil {
		log.Printf("[Firewall] mDNS rule (UDP 5353) failed — LAN discovery may not work: %v", err)
	}

	return writeFirewallFlag(flagPath)
}

func ensureOneRule(name string, port int) error {
	// Check if a rule already exists (works without elevation).
	check := exec.Command("netsh", "advfirewall", "firewall", "show", "rule", "name="+name)
	if out, err := check.CombinedOutput(); err == nil && strings.Contains(string(out), name) {
		log.Printf("[Firewall] rule %q already exists, skipping", name)
		return nil
	}

	// Add inbound UDP rule — port-only, no program= filter.
	// This ensures the rule works regardless of where the binary lives.
	args := []string{"advfirewall", "firewall", "add", "rule",
		"name=" + name,
		"dir=in",
		"action=allow",
		"enable=yes",
		"protocol=udp",
		fmt.Sprintf("localport=%d", port),
	}

	// Try direct first (might already be admin).
	cmd := exec.Command("netsh", args...)
	if err := cmd.Run(); err == nil {
		log.Printf("[Firewall] UDP port %d allowed (inbound, rule %q)", port, name)
		return nil
	}

	// Not admin — use ShellExecuteW "runas" for UAC elevation.
	// This shows the standard Windows UAC dialog once on first run.
	verb, _ := syscall.UTF16PtrFromString("runas")
	exe, _ := syscall.UTF16PtrFromString("netsh")
	argStr, _ := syscall.UTF16PtrFromString(strings.Join(args, " "))
	cwd, _ := syscall.UTF16PtrFromString(".")

	shell32 := syscall.NewLazyDLL("shell32.dll")
	proc := shell32.NewProc("ShellExecuteW")
	ret, _, _ := proc.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(exe)),
		uintptr(unsafe.Pointer(argStr)),
		uintptr(unsafe.Pointer(cwd)),
		0, // SW_HIDE
	)
	if ret <= 32 {
		return fmt.Errorf("firewall rule %q failed — please run once as Administrator, or manually allow UDP port %d", name, port)
	}

	log.Printf("[Firewall] UDP port %d allowed via UAC elevation (rule %q)", port, name)
	return nil
}
