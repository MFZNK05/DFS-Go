//go:build !windows

package cmd

import (
	"os/exec"
	"syscall"
)

// detachCmd configures an exec.Cmd to run as a fully detached background
// process on Unix (new session, no controlling terminal).
func detachCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}
