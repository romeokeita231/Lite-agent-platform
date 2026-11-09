//go:build !windows

package demo

import "os/exec"

type processGroup struct{}

func startInProcessGroup(*exec.Cmd) (*processGroup, error) {
	return &processGroup{}, nil
}

func (g *processGroup) close() error {
	return nil
}
