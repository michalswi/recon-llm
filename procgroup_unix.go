//go:build darwin || linux

package main

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup makes cmd the leader of a new process group so that
// killProcessGroup can terminate every process it spawns (e.g. a background
// "bash -c" script that starts its own children), not just the shell itself.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills cmd's entire process group; falls back to killing
// only the top-level process if the group signal cannot be delivered.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
