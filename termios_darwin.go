//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

// darwinTermios mirrors struct termios from <sys/termios.h> on 64-bit Darwin.
type darwinTermios struct {
	Iflag  uint64
	Oflag  uint64
	Cflag  uint64
	Lflag  uint64
	Cc     [20]byte
	Ispeed uint64
	Ospeed uint64
}

type darwinWinsize struct {
	Rows    uint16
	Columns uint16
	XPixel  uint16
	YPixel  uint16
}

const (
	darwinTIOCGETA   = 0x40487413
	darwinTIOCSETA   = 0x80487414
	darwinTIOCGWINSZ = 0x40087468

	darwinICRNL  = 0x00000100
	darwinIXON   = 0x00000200
	darwinOPOST  = 0x00000001
	darwinECHO   = 0x00000008
	darwinICANON = 0x00000100
	darwinISIG   = 0x00000080
	darwinIEXTEN = 0x00000400

	darwinVMIN  = 16
	darwinVTIME = 17
)

func darwinIoctl(fd int, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// platformEnableRawMode disables canonical mode, echo, and signal generation so
// individual keystrokes (including arrow-key escape sequences) can be read directly.
func platformEnableRawMode(fd int) (func() error, error) {
	var original darwinTermios
	if err := darwinIoctl(fd, darwinTIOCGETA, unsafe.Pointer(&original)); err != nil {
		return nil, err
	}
	raw := original
	raw.Iflag &^= uint64(darwinICRNL | darwinIXON)
	raw.Oflag &^= uint64(darwinOPOST)
	raw.Lflag &^= uint64(darwinECHO | darwinICANON | darwinISIG | darwinIEXTEN)
	raw.Cc[darwinVMIN] = 1
	raw.Cc[darwinVTIME] = 0
	if err := darwinIoctl(fd, darwinTIOCSETA, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	return func() error {
		return darwinIoctl(fd, darwinTIOCSETA, unsafe.Pointer(&original))
	}, nil
}

func platformTerminalWidth(fd int) int {
	var size darwinWinsize
	if err := darwinIoctl(fd, darwinTIOCGWINSZ, unsafe.Pointer(&size)); err != nil || size.Columns == 0 {
		return 80
	}
	return int(size.Columns)
}
