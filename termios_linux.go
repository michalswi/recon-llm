//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

// linuxTermios mirrors the kernel ioctl struct termios (asm-generic/termbits.h)
// used on amd64 and arm64, as returned by TCGETS/TCSETS.
type linuxTermios struct {
	Iflag  uint32
	Oflag  uint32
	Cflag  uint32
	Lflag  uint32
	Line   byte
	Cc     [19]byte
	Ispeed uint32
	Ospeed uint32
}

type linuxWinsize struct {
	Rows    uint16
	Columns uint16
	XPixel  uint16
	YPixel  uint16
}

const (
	linuxTCGETS     = 0x5401
	linuxTCSETS     = 0x5402
	linuxTIOCGWINSZ = 0x5413

	linuxICRNL  = 0x00000100
	linuxIXON   = 0x00000400
	linuxOPOST  = 0x00000001
	linuxECHO   = 0x00000008
	linuxICANON = 0x00000002
	linuxISIG   = 0x00000001
	linuxIEXTEN = 0x00008000

	linuxVMIN  = 6
	linuxVTIME = 5
)

func linuxIoctl(fd int, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// platformEnableRawMode disables canonical mode, echo, and signal generation so
// individual keystrokes (including arrow-key escape sequences) can be read directly.
func platformEnableRawMode(fd int) (func() error, error) {
	var original linuxTermios
	if err := linuxIoctl(fd, linuxTCGETS, unsafe.Pointer(&original)); err != nil {
		return nil, err
	}
	raw := original
	raw.Iflag &^= uint32(linuxICRNL | linuxIXON)
	raw.Oflag &^= uint32(linuxOPOST)
	raw.Lflag &^= uint32(linuxECHO | linuxICANON | linuxISIG | linuxIEXTEN)
	raw.Cc[linuxVMIN] = 1
	raw.Cc[linuxVTIME] = 0
	if err := linuxIoctl(fd, linuxTCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	return func() error {
		return linuxIoctl(fd, linuxTCSETS, unsafe.Pointer(&original))
	}, nil
}

func platformTerminalWidth(fd int) int {
	var size linuxWinsize
	if err := linuxIoctl(fd, linuxTIOCGWINSZ, unsafe.Pointer(&size)); err != nil || size.Columns == 0 {
		return 80
	}
	return int(size.Columns)
}
