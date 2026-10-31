//go:build windows

package sidecar

import (
	"io"
	"os"
	"os/exec"
)

var forwardedSignals = []os.Signal{os.Interrupt}

func isolateChild(*exec.Cmd, io.Reader) {}

func codeFromExit(e *exec.ExitError) int { return e.ExitCode() }
