package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

func TestPlatformDemoStartsAndStopsTheWholeStack(t *testing.T) {
	root := demoRepoRoot(t)
	postgresBin := demoPostgresBinDir(t, root)
	workspace := t.TempDir()
	binDir := filepath.Join(workspace, "bin")
	dataDir := filepath.Join(workspace, "data")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create bin dir: %v", err)
	}
	platformBin := demoBinaryPath(binDir, "platform")
	demoBuildBinary(t, root, platformBin, "./cmd/platform")
	demoBuildBinary(t, root, demoBinaryPath(binDir, "policy-example"), "./cmd/policy-example")

	logPath := filepath.Join(workspace, "platform.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log file: %v", err)
	}

	cmd := exec.Command(platformBin, "demo")
	runDir := filepath.Join(workspace, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("create working directory: %v", err)
	}
	cmd.Dir = runDir
	cmd.SysProcAttr = shutdownSysProcAttr()
	cmd.Env = append(os.Environ(),
		"HTTP_ADDR=:18080",
		"DATABASE_URL=postgres://postgres:postgres@127.0.0.1:15433/platform?sslmode=disable",
		"POLICY_ADDR=:19090",
		"DEMO_DATA_DIR="+dataDir,
		"DEMO_POSTGRES_BIN_DIR="+postgresBin,
		"METRICS_ADDR=127.0.0.1:19091",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start platform demo: %v", err)
	}

	finished := false
	t.Cleanup(func() {
		if !finished {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}
		_ = logFile.Close()
		if t.Failed() {
			if raw, err := os.ReadFile(logPath); err == nil {
				t.Logf("platform demo log:\n%s", raw)
			}
		}
	})

	base := "http://127.0.0.1:18080"
	demoWaitForEndpoints(t, base, 40*time.Second)

	proc := &platformProcess{cmd: cmd, log: logFile, path: logPath}
	signalShutdown(t, proc)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		finished = true
		if err != nil {
			t.Fatalf("platform demo exited with error after interrupt: %v", err)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		finished = true
		t.Fatalf("platform demo did not exit after interrupt")
	}

	for _, target := range []string{"127.0.0.1:18080", "127.0.0.1:15433", "127.0.0.1:19090"} {
		demoWaitForFreePort(t, target, 5*time.Second)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read demo log: %v", err)
	}
	logged := string(raw)
	for _, want := range []string{"demo-a-api-key", "demo-b-api-key", "http://127.0.0.1:18080/console"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("demo log does not print %q:\n%s", want, logged)
		}
	}
}

func demoWaitForEndpoints(t *testing.T, base string, timeout time.Duration) {
	t.Helper()
	paths := []string{"/healthz", "/readyz", "/console"}
	pending := map[string]bool{"/healthz": true, "/readyz": true, "/console": true}
	last := map[string]string{}
	deadline := time.Now().Add(timeout)
	for {
		all := true
		for _, path := range paths {
			if !pending[path] {
				continue
			}
			status, err := demoGet(base + path)
			if err != nil {
				last[path] = err.Error()
				all = false
				continue
			}
			if status != http.StatusOK {
				last[path] = fmt.Sprintf("status %d", status)
				all = false
				continue
			}
			pending[path] = false
		}
		if all {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("demo endpoints did not become ready within %s: %v", timeout, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func demoGet(target string) (int, error) {
	resp, err := http.Get(target)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func demoWaitForFreePort(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		listener, err := net.Listen("tcp", addr)
		if err == nil {
			_ = listener.Close()
			return
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("port %s is still in use after shutdown: %v", addr, lastErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func demoRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

func demoBinaryPath(dir, name string) string {
	if goruntime.GOOS == "windows" {
		return filepath.Join(dir, name+".exe")
	}
	return filepath.Join(dir, name)
}

func demoBuildBinary(t *testing.T, root, target, pkg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", target, pkg)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
}

func demoPostgresBinDir(t *testing.T, root string) string {
	t.Helper()
	candidates := []string{}
	if dir := os.Getenv("DEMO_POSTGRES_BIN_DIR"); dir != "" {
		candidates = append(candidates, dir)
	}
	defaultDir := filepath.Join(root, "dist", "postgres", "bin")
	candidates = append(candidates, defaultDir)
	for _, dir := range candidates {
		complete := true
		for _, binary := range []string{"initdb", "postgres"} {
			name := binary
			if goruntime.GOOS == "windows" {
				name += ".exe"
			}
			if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.IsDir() {
				complete = false
				break
			}
		}
		if complete {
			return dir
		}
	}
	t.Fatalf("Postgres binaries are missing: provision %s (initdb and postgres) or set DEMO_POSTGRES_BIN_DIR to a Postgres bin directory",
		defaultDir)
	return ""
}
