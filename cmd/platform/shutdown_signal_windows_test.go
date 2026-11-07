//go:build windows

package main

import (
	"syscall"
	"testing"
)

func shutdownSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func signalShutdown(t *testing.T, proc *platformProcess) {
	t.Helper()
	kernel32, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		t.Fatalf("load kernel32: %v", err)
	}
	generate, err := kernel32.FindProc("GenerateConsoleCtrlEvent")
	if err != nil {
		t.Fatalf("find GenerateConsoleCtrlEvent: %v", err)
	}
	result, _, callErr := generate.Call(syscall.CTRL_BREAK_EVENT, uintptr(proc.cmd.Process.Pid))
	if result == 0 {
		t.Fatalf("GenerateConsoleCtrlEvent: %v", callErr)
	}
}
