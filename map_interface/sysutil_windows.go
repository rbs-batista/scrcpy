//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"unsafe"
)

// setProcAttr hides the subprocess console window on Windows.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// setWindowIcon loads icon resource ID 1 from the running exe and applies it
// to the webview window so the title bar and taskbar show the correct icon.
func setWindowIcon(handle unsafe.Pointer) {
	if handle == nil {
		return
	}
	hwnd := uintptr(handle)

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	user32 := syscall.NewLazyDLL("user32.dll")
	getModuleHandle := kernel32.NewProc("GetModuleHandleW")
	loadImage := user32.NewProc("LoadImageW")
	sendMessage := user32.NewProc("SendMessageW")

	hInst, _, _ := getModuleHandle.Call(0)

	const (
		imageIcon     = 1
		lrDefaultSize = 0x0040
		lrShared      = 0x8000
		wmSetIcon     = 0x0080
		iconSmall     = 0
		iconBig       = 1
	)

	bigIcon, _, _ := loadImage.Call(hInst, 1, imageIcon, 32, 32, lrDefaultSize|lrShared)
	smallIcon, _, _ := loadImage.Call(hInst, 1, imageIcon, 16, 16, lrDefaultSize|lrShared)

	sendMessage.Call(hwnd, wmSetIcon, iconBig, bigIcon)
	sendMessage.Call(hwnd, wmSetIcon, iconSmall, smallIcon)
}
