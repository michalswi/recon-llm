//go:build !darwin && !linux

package main

import "os/exec"

// configureProcessGroup is a no-op on platforms without POSIX process groups.
func configureProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup falls back to killing only the top-level process on this platform.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
