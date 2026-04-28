//go:build !windows

package main

import (
	"os/exec"
	"unsafe"
)

func setProcAttr(cmd *exec.Cmd) {}

func setWindowIcon(_ unsafe.Pointer) {}
