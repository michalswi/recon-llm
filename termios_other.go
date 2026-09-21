//go:build !darwin && !linux

package main

import "errors"

// platformEnableRawMode is unsupported on this platform; callers fall back to
// plain line reading with no arrow-key editing or history navigation.
func platformEnableRawMode(fd int) (func() error, error) {
	return nil, errors.New("raw terminal mode is not supported on this platform")
}

func platformTerminalWidth(fd int) int {
	return 80
}
