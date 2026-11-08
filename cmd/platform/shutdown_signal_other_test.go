//go:build !windows

package main

import (
	"os"
	"syscall"
	"testing"
)

func shutdownSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func signalShutdown(t *testing.T, proc *platformProcess) {
	t.Helper()
	if err := proc.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal platform: %v", err)
	}
}
