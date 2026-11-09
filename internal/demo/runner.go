package demo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

type ExecRunner struct{}

func (ExecRunner) Start(ctx context.Context, spec ChildSpec) (Child, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := exec.Command(spec.Path, spec.Args...)
	command.Dir = spec.Dir
	command.Env = append(os.Environ(), spec.Env...)
	command.Stdout = io.Discard

	child := &execChild{name: spec.Name, done: make(chan struct{}), logPath: spec.LogPath}
	if spec.LogPath != "" {
		logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open %s log %s: %w", spec.Name, spec.LogPath, err)
		}
		child.logFile = logFile
		command.Stderr = logFile
	} else {
		child.stderr = &bytes.Buffer{}
		command.Stderr = child.stderr
	}
	child.command = command
	if err := command.Start(); err != nil {
		if child.logFile != nil {
			_ = child.logFile.Close()
		}
		return nil, fmt.Errorf("start %s: %w", spec.Name, err)
	}
	group, err := startInProcessGroup(command)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		if child.logFile != nil {
			_ = child.logFile.Close()
		}
		return nil, fmt.Errorf("start %s: %w", spec.Name, err)
	}
	child.group = group
	go child.wait()
	return child, nil
}

type execChild struct {
	name    string
	command *exec.Cmd
	stderr  *bytes.Buffer
	logFile *os.File
	logPath string
	group   *processGroup

	waitOnce sync.Once
	waitErr  error
	done     chan struct{}
}

func (c *execChild) wait() {
	c.waitOnce.Do(func() {
		c.waitErr = c.command.Wait()
		if c.logFile != nil {
			_ = c.logFile.Close()
		}
		_ = c.group.close()
		close(c.done)
	})
}

func (c *execChild) Wait() error {
	c.wait()
	<-c.done
	if c.waitErr == nil {
		return nil
	}
	return fmt.Errorf("%s: %w%s", c.name, c.waitErr, c.diagnostic())
}

func (c *execChild) Stop(ctx context.Context) error {
	select {
	case <-c.done:
		return nil
	default:
	}
	process := c.command.Process
	if process != nil {
		if err := process.Signal(os.Interrupt); err != nil {
			_ = process.Kill()
		}
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		if process != nil {
			_ = process.Kill()
		}
		<-c.done
		return ctx.Err()
	}
}

func (c *execChild) diagnostic() string {
	var message string
	if c.stderr != nil {
		message = c.stderr.String()
	} else if c.logPath != "" {
		if data, err := os.ReadFile(c.logPath); err == nil {
			message = string(data)
		}
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	const limit = 2048
	if len(message) > limit {
		message = message[len(message)-limit:]
	}
	return ": " + message
}
