package main

import (
	"io"

	"github.com/romeokeita231/Lite-agent-platform/internal/seed"
)

func runSeed(w io.Writer) error {
	_, err := seed.Run(w)
	return err
}
