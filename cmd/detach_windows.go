//go:build windows

package cmd

import (
	"os/exec"
	"syscall"
)

// detachCmd configures an exec.Cmd to run as a detached background
// process on Windows (new process group, detached from console).
func detachCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}
