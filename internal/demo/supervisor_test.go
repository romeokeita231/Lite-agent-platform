package demo

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type fakeRunner struct {
	mu        sync.Mutex
	specs     []ChildSpec
	children  []*fakeChild
	startErrs map[string]error
	waitErrs  map[string]error
	stops     []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{startErrs: map[string]error{}, waitErrs: map[string]error{}}
}

func (r *fakeRunner) Start(ctx context.Context, spec ChildSpec) (Child, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := filepath.Base(spec.Path)
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	startErr := r.startErrs[name]
	waitErr := r.waitErrs[name]
	r.mu.Unlock()
	if startErr != nil {
		return nil, startErr
	}
	child := &fakeChild{name: name, runner: r, waitErr: waitErr, done: make(chan struct{})}
	r.mu.Lock()
	r.children = append(r.children, child)
	r.mu.Unlock()
	return child, nil
}

func (r *fakeRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.specs)
}

func (r *fakeRunner) startedNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.specs))
	for _, spec := range r.specs {
		names = append(names, filepath.Base(spec.Path))
	}
	return names
}

func (r *fakeRunner) specFor(name string) (ChildSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, spec := range r.specs {
		if filepath.Base(spec.Path) == name {
			return spec, true
		}
	}
	return ChildSpec{}, false
}

func (r *fakeRunner) activeChildren() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := []string{}
	for _, child := range r.children {
		if child.active() {
			names = append(names, child.name)
		}
	}
	return names
}

func (r *fakeRunner) stopOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.stops...)
}

func (r *fakeRunner) recordStop(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops = append(r.stops, name)
}

type fakeChild struct {
	name     string
	runner   *fakeRunner
	waitErr  error
	done     chan struct{}
	finish   sync.Once
	stopped  bool
	stopOnce sync.Once
}

func (c *fakeChild) Wait() error {
	c.finish.Do(func() { close(c.done) })
	return c.waitErr
}

func (c *fakeChild) Stop(ctx context.Context) error {
	c.stopOnce.Do(func() {
		c.stopped = true
		c.runner.recordStop(c.name)
		c.finish.Do(func() { close(c.done) })
	})
	return nil
}

func (c *fakeChild) active() bool {
	select {
	case <-c.done:
		return false
	default:
		return !c.stopped
	}
}

func noProbe(context.Context, ChildSpec) error {
	return nil
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func databaseURL(port int) string {
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/platform?sslmode=disable", port)
}

func testOptions(t *testing.T, runner Runner, dbPort int) Options {
	t.Helper()
	return Options{
		DataDir:        t.TempDir(),
		PostgresBinDir: t.TempDir(),
		Runner:         runner,
		Probe:          noProbe,
		DatabaseURL:    databaseURL(dbPort),
		PolicyAddr:     "127.0.0.1:" + strconv.Itoa(freePort(t)),
	}
}

func TestStartDatabaseAbortsWhenPortIsOccupied(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	runner := newFakeRunner()
	supervisor, err := New(testOptions(t, runner, port))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = supervisor.StartDatabase(context.Background())
	if err == nil {
		t.Fatal("StartDatabase succeeded on an occupied port")
	}
	message := err.Error()
	for _, want := range []string{"start database", strconv.Itoa(port), "already in use", "DATABASE_URL"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not mention %q", message, want)
		}
	}
	if runner.startCount() != 0 {
		t.Fatalf("children were started after the port check failed: %v", runner.startedNames())
	}
	if active := runner.activeChildren(); len(active) != 0 {
		t.Fatalf("half-started children remain: %v", active)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestInitdbVCRuntimeFailureIsHumanReadable(t *testing.T) {
	runner := newFakeRunner()
	runner.waitErrs[binaryName("initdb")] = errors.New("initdb: exit status 3221225781")

	supervisor, err := New(testOptions(t, runner, freePort(t)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = supervisor.StartDatabase(context.Background())
	if err == nil {
		t.Fatal("StartDatabase succeeded when initdb failed")
	}
	message := err.Error()
	for _, want := range []string{"start database", "Visual C++ runtime", vcRedistURL} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not mention %q", message, want)
		}
	}
	for _, raw := range []string{"3221225781", "VCRUNTIME", "exit status"} {
		if strings.Contains(message, raw) {
			t.Fatalf("error %q leaks raw initdb output %q", message, raw)
		}
	}
	if active := runner.activeChildren(); len(active) != 0 {
		t.Fatalf("half-started children remain: %v", active)
	}
}

func TestInitdbGenericFailureNamesStepAndRemedy(t *testing.T) {
	runner := newFakeRunner()
	runner.waitErrs[binaryName("initdb")] = errors.New("initdb: could not create the data directory")

	supervisor, err := New(testOptions(t, runner, freePort(t)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = supervisor.StartDatabase(context.Background())
	if err == nil {
		t.Fatal("StartDatabase succeeded when initdb failed")
	}
	message := err.Error()
	for _, want := range []string{"start database", "initdb", "--reset"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not mention %q", message, want)
		}
	}
	if active := runner.activeChildren(); len(active) != 0 {
		t.Fatalf("half-started children remain: %v", active)
	}
}

func TestPolicyStartFailureAbortsTheWholeStartup(t *testing.T) {
	runner := newFakeRunner()
	runner.startErrs[binaryName("policy-example")] = errors.New("exec: file does not exist")

	opts := testOptions(t, runner, freePort(t))
	opts.ExecutablePath = filepath.Join(t.TempDir(), binaryName("platform"))
	supervisor, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := supervisor.StartDatabase(context.Background()); err != nil {
		t.Fatalf("StartDatabase: %v", err)
	}
	err = supervisor.StartPolicy(context.Background())
	if err == nil {
		t.Fatal("StartPolicy succeeded when the policy could not start")
	}
	message := err.Error()
	for _, want := range []string{"start policy", binaryName("policy-example"), "next to the running executable"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not mention %q", message, want)
		}
	}
	if active := runner.activeChildren(); len(active) != 0 {
		t.Fatalf("half-started children remain: %v", active)
	}
	stops := runner.stopOrder()
	if len(stops) == 0 || stops[len(stops)-1] != binaryName("postgres") {
		t.Fatalf("database child was not stopped after the policy failure: %v", stops)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if second := runner.stopOrder(); len(second) != len(stops) {
		t.Fatalf("Stop is not idempotent: %v then %v", stops, second)
	}
}

func TestPolicyStartsBesideExecutableAndStopIsReverseOrdered(t *testing.T) {
	runner := newFakeRunner()
	base := t.TempDir()
	opts := testOptions(t, runner, freePort(t))
	opts.ExecutablePath = filepath.Join(base, binaryName("platform"))
	opts.PolicySecret = ""
	t.Setenv("POLICY_SECRET", "")
	supervisor, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := supervisor.StartDatabase(context.Background()); err != nil {
		t.Fatalf("StartDatabase: %v", err)
	}
	if err := supervisor.StartPolicy(context.Background()); err != nil {
		t.Fatalf("StartPolicy: %v", err)
	}
	spec, ok := runner.specFor(binaryName("policy-example"))
	if !ok {
		t.Fatalf("policy child was not started: %v", runner.startedNames())
	}
	if want := filepath.Join(base, binaryName("policy-example")); spec.Path != want {
		t.Fatalf("policy path = %q, want %q", spec.Path, want)
	}
	if spec.Dir != supervisor.DataDir() {
		t.Fatalf("policy working directory = %q, want %q", spec.Dir, supervisor.DataDir())
	}
	wants := []string{"POLICY_ADDR=" + opts.PolicyAddr, "POLICY_SECRET=" + defaultPolicySecret}
	for _, want := range wants {
		found := false
		for _, env := range spec.Env {
			if env == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("policy env %v does not contain %q", spec.Env, want)
		}
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	wantStops := []string{binaryName("policy-example"), binaryName("postgres")}
	if got := runner.stopOrder(); len(got) != len(wantStops) || got[0] != wantStops[0] || got[1] != wantStops[1] {
		t.Fatalf("stop order = %v, want %v", got, wantStops)
	}
}

func TestDataDirDefaultsAndEnvironmentOverrides(t *testing.T) {
	localAppData := t.TempDir()
	t.Setenv("LOCALAPPDATA", localAppData)
	t.Setenv("DEMO_DATA_DIR", "")

	supervisor, err := New(Options{Runner: newFakeRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wantDefault := filepath.Join(localAppData, "LiteAgentPlatform", "demo")
	if supervisor.DataDir() != wantDefault {
		t.Fatalf("default data dir = %q, want %q", supervisor.DataDir(), wantDefault)
	}

	envDir := t.TempDir()
	t.Setenv("DEMO_DATA_DIR", envDir)
	supervisor, err = New(Options{Runner: newFakeRunner()})
	if err != nil {
		t.Fatalf("New with DEMO_DATA_DIR: %v", err)
	}
	if supervisor.DataDir() != envDir {
		t.Fatalf("DEMO_DATA_DIR data dir = %q, want %q", supervisor.DataDir(), envDir)
	}

	explicit := t.TempDir()
	supervisor, err = New(Options{DataDir: explicit, Runner: newFakeRunner()})
	if err != nil {
		t.Fatalf("New with explicit data dir: %v", err)
	}
	if supervisor.DataDir() != explicit {
		t.Fatalf("explicit data dir = %q, want %q", supervisor.DataDir(), explicit)
	}
}

func TestResetClearsAndRebuildsTheDataDirectory(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, []byte("old"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	supervisor, err := New(Options{
		DataDir:        dir,
		Reset:          true,
		PostgresBinDir: t.TempDir(),
		Runner:         newFakeRunner(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := supervisor.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker still exists after --reset: %v", err)
	}
	for _, sub := range []string{"data", "logs"} {
		if info, err := os.Stat(filepath.Join(dir, sub)); err != nil || !info.IsDir() {
			t.Fatalf("%s was not rebuilt: %v", sub, err)
		}
	}
}

func TestPrepareRefusesToWriteInsideTheRepository(t *testing.T) {
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	inside := filepath.Join(repo, "demo-test-must-not-write")
	t.Cleanup(func() { _ = os.RemoveAll(inside) })
	supervisor, err := New(Options{
		DataDir:        inside,
		PostgresBinDir: t.TempDir(),
		Runner:         newFakeRunner(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = supervisor.Prepare(context.Background())
	if err == nil {
		t.Fatal("Prepare wrote inside the repository")
	}
	message := err.Error()
	for _, want := range []string{"prepare demo data directory", "repository", "DEMO_DATA_DIR"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not mention %q", message, want)
		}
	}
	if _, statErr := os.Stat(inside); !os.IsNotExist(statErr) {
		t.Fatalf("repository directory was written: %v", statErr)
	}
}

func TestPrepareExtractsThePostgresArchive(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "postgres.zip")
	writeZip(t, archive, map[string]string{
		"postgres/bin/initdb.exe":   "initdb",
		"postgres/bin/postgres.exe": "postgres",
	})
	dataDir := t.TempDir()
	supervisor, err := New(Options{
		DataDir:         dataDir,
		PostgresArchive: archive,
		Runner:          newFakeRunner(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := supervisor.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	initdb := filepath.Join(dataDir, "postgres", "postgres", "bin", binaryName("initdb"))
	if _, err := os.Stat(initdb); err != nil {
		t.Fatalf("initdb was not extracted to %s: %v", initdb, err)
	}
}

func TestPortsAreDefaultsOrExplicitEnvironmentValues(t *testing.T) {
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("POLICY_ADDR", "")

	supervisor, err := New(Options{Runner: newFakeRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ports := supervisor.Ports()
	if ports.HTTPAddr != defaultHTTPAddr || ports.PolicyAddr != defaultPolicyAddr || ports.DatabaseURL != DefaultDatabaseURL {
		t.Fatalf("default ports = %+v", ports)
	}

	overrides := Ports{
		HTTPAddr:    ":18080",
		DatabaseURL: databaseURL(15433),
		PolicyAddr:  ":19090",
	}
	t.Setenv("HTTP_ADDR", overrides.HTTPAddr)
	t.Setenv("DATABASE_URL", overrides.DatabaseURL)
	t.Setenv("POLICY_ADDR", overrides.PolicyAddr)
	supervisor, err = New(Options{Runner: newFakeRunner()})
	if err != nil {
		t.Fatalf("New with overrides: %v", err)
	}
	if got := supervisor.Ports(); got != overrides {
		t.Fatalf("overridden ports = %+v, want %+v", got, overrides)
	}
}

func TestConfiguredPortsAreReportedWithoutProbing(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	occupied := databaseURL(port)
	t.Setenv("DATABASE_URL", occupied)
	t.Setenv("HTTP_ADDR", ":8080")
	t.Setenv("POLICY_ADDR", ":9090")

	supervisor, err := New(Options{
		DataDir:        t.TempDir(),
		PostgresBinDir: t.TempDir(),
		Runner:         newFakeRunner(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := supervisor.Ports().DatabaseURL; got != occupied {
		t.Fatalf("DATABASE_URL drifted to %q, want %q", got, occupied)
	}
	if err := supervisor.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare touched the configured ports: %v", err)
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer file.Close()
	writer := zip.NewWriter(file)
	for name, body := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
}
