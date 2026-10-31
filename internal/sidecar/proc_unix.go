//go:build !windows

package sidecar

import (
	"io"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

var forwardedSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

// isolateChild puts the child in its own process group. Terminal Ctrl-C and CI
// runners signal a whole group, and Terraform treats a second SIGINT as "abort
// immediately", so a child sharing our group would get the signal twice: once
// directly and once forwarded. On a terminal the child's group also becomes the
// foreground group, or its prompts would stop with SIGTTIN.
func isolateChild(cmd *exec.Cmd, stdin io.Reader) {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if f, ok := stdin.(*os.File); ok && inForeground(f) {
		attr.Foreground = true
		attr.Ctty = int(f.Fd())
	}
	cmd.SysProcAttr = attr
}

// inForeground reports whether f is a terminal whose foreground group is ours.
func inForeground(f *os.File) bool {
	var pgrp int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&pgrp))) //nolint:gosec // TIOCGPGRP needs a pointer
	return errno == 0 && int(pgrp) == syscall.Getpgrp()
}

func codeFromExit(e *exec.ExitError) int {
	if ws, ok := e.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return e.ExitCode()
}
