package demo

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultHTTPAddr     = ":8080"
	defaultPolicyAddr   = ":9090"
	defaultPolicySecret = "demo-policy-secret"
	defaultReadyTimeout = 30 * time.Second

	DefaultDatabaseURL = "postgres://postgres:postgres@127.0.0.1:5433/platform?sslmode=disable"

	vcRedistURL = "https://learn.microsoft.com/en-us/cpp/windows/latest-supported-vc-redist"
)

type ChildSpec struct {
	Name         string
	Path         string
	Args         []string
	Env          []string
	Dir          string
	LogPath      string
	ReadyAddress string
	ReadyTimeout time.Duration
}

type Runner interface {
	Start(ctx context.Context, spec ChildSpec) (Child, error)
}

type Child interface {
	Wait() error
	Stop(ctx context.Context) error
}

type Ports struct {
	HTTPAddr    string
	DatabaseURL string
	PolicyAddr  string
}

type Options struct {
	DataDir         string
	Reset           bool
	Runner          Runner
	PostgresBinDir  string
	PostgresArchive string
	PolicyPath      string
	ExecutablePath  string
	HTTPAddr        string
	DatabaseURL     string
	PolicyAddr      string
	PolicySecret    string
	Probe           func(ctx context.Context, spec ChildSpec) error
}

type Supervisor struct {
	opts         Options
	ports        Ports
	dataDir      string
	policySecret string
	probe        func(ctx context.Context, spec ChildSpec) error

	mu       sync.Mutex
	children []runningChild
	prepared bool

	postgresBin string
}

type runningChild struct {
	name  string
	child Child
}

type databaseConfig struct {
	host     string
	port     int
	user     string
	database string
}

func New(opts Options) (*Supervisor, error) {
	ports, err := resolvePorts(opts)
	if err != nil {
		return nil, err
	}
	dataDir, err := resolveDataDir(opts.DataDir)
	if err != nil {
		return nil, err
	}
	if opts.Runner == nil {
		opts.Runner = ExecRunner{}
	}
	policySecret := opts.PolicySecret
	if policySecret == "" {
		policySecret = os.Getenv("POLICY_SECRET")
	}
	if policySecret == "" {
		policySecret = defaultPolicySecret
	}
	probe := opts.Probe
	if probe == nil {
		probe = tcpProbe
	}
	return &Supervisor{
		opts:         opts,
		ports:        ports,
		dataDir:      dataDir,
		policySecret: policySecret,
		probe:        probe,
	}, nil
}

func (s *Supervisor) Ports() Ports {
	return s.ports
}

func (s *Supervisor) DataDir() string {
	return s.dataDir
}

func (s *Supervisor) Prepare(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.prepared {
		return nil
	}
	absCwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("prepare demo data directory: cannot determine the repository directory: %w", err)
	}
	if err := ensureOutsideRepository(s.dataDir, absCwd); err != nil {
		return err
	}
	if exe, err := s.executablePath(); err == nil {
		if err := ensureOutsideRepository(s.dataDir, filepath.Dir(exe)); err != nil {
			return err
		}
	}
	if s.opts.Reset {
		if err := os.RemoveAll(s.dataDir); err != nil {
			return fmt.Errorf("prepare demo data directory: cannot clear %s: %w", s.dataDir, err)
		}
	}
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return fmt.Errorf("prepare demo data directory: cannot create %s: %w", s.dataDir, err)
	}
	if err := os.MkdirAll(filepath.Join(s.dataDir, "data"), 0o755); err != nil {
		return fmt.Errorf("prepare demo data directory: cannot create the Postgres data directory under %s: %w", s.dataDir, err)
	}
	if err := os.MkdirAll(filepath.Join(s.dataDir, "logs"), 0o755); err != nil {
		return fmt.Errorf("prepare demo data directory: cannot create the log directory under %s: %w", s.dataDir, err)
	}
	binDir, err := s.preparePostgres()
	if err != nil {
		return err
	}
	s.postgresBin = binDir
	s.prepared = true
	return nil
}

func (s *Supervisor) StartDatabase(ctx context.Context) (string, error) {
	if err := s.Prepare(ctx); err != nil {
		s.abort(ctx)
		return "", err
	}
	db, err := parseDatabaseURL(s.ports.DatabaseURL)
	if err != nil {
		s.abort(ctx)
		return "", fmt.Errorf("start database: DATABASE_URL is invalid: %w", err)
	}
	addr := net.JoinHostPort(db.host, strconv.Itoa(db.port))
	if err := checkAddressFree(addr); err != nil {
		s.abort(ctx)
		return "", stepFailure("start database",
			fmt.Sprintf("port %d is already in use by another process", db.port),
			"stop the process using it (the compose stack uses ports 8080 and 9090) or set DATABASE_URL to a different port")
	}
	pgdata := filepath.Join(s.dataDir, "data")
	if err := s.initDatabaseIfNeeded(ctx, pgdata, db); err != nil {
		s.abort(ctx)
		return "", err
	}
	spec := ChildSpec{
		Name:         "postgres",
		Path:         filepath.Join(s.postgresBin, binaryName("postgres")),
		Args:         []string{"-D", pgdata, "-p", strconv.Itoa(db.port), "-h", db.host},
		Env:          []string{},
		Dir:          s.dataDir,
		LogPath:      filepath.Join(s.dataDir, "logs", "postgres.log"),
		ReadyAddress: net.JoinHostPort(db.host, strconv.Itoa(db.port)),
		ReadyTimeout: defaultReadyTimeout,
	}
	child, err := s.opts.Runner.Start(ctx, spec)
	if err != nil {
		s.abort(ctx)
		return "", s.databaseFailure(spec, err)
	}
	s.addChild("database", child)
	if err := s.probe(ctx, spec); err != nil {
		s.abort(ctx)
		return "", stepFailure("start database",
			fmt.Sprintf("Postgres did not accept connections on %s", addr),
			"inspect "+spec.LogPath+" or run --reset to rebuild the data directory")
	}
	if err := s.createDatabase(ctx, db); err != nil {
		s.abort(ctx)
		return "", err
	}
	return s.ports.DatabaseURL, nil
}

func (s *Supervisor) StartPolicy(ctx context.Context) error {
	if err := s.Prepare(ctx); err != nil {
		s.abort(ctx)
		return err
	}
	path, err := s.resolvePolicyPath()
	if err != nil {
		s.abort(ctx)
		return err
	}
	if err := checkAddressFree(s.ports.PolicyAddr); err != nil {
		s.abort(ctx)
		_, port, splitErr := net.SplitHostPort(s.ports.PolicyAddr)
		if splitErr != nil {
			port = s.ports.PolicyAddr
		}
		return stepFailure("start policy",
			fmt.Sprintf("port %s is already in use by another process", port),
			"stop the process using it (the compose stack uses port 9090) or set POLICY_ADDR to a different port")
	}
	spec := ChildSpec{
		Name:         "policy",
		Path:         path,
		Env:          []string{"POLICY_ADDR=" + s.ports.PolicyAddr, "POLICY_SECRET=" + s.policySecret},
		Dir:          s.dataDir,
		LogPath:      filepath.Join(s.dataDir, "logs", "policy.log"),
		ReadyAddress: probeAddress(s.ports.PolicyAddr),
		ReadyTimeout: defaultReadyTimeout,
	}
	child, err := s.opts.Runner.Start(ctx, spec)
	if err != nil {
		s.abort(ctx)
		return stepFailureCause("start policy",
			path+" could not be started",
			"ensure "+binaryName("policy-example")+" sits next to the running executable or set Options.PolicyPath", err)
	}
	s.addChild("policy", child)
	if err := s.probe(ctx, spec); err != nil {
		s.abort(ctx)
		return stepFailure("start policy",
			"the Policy did not become ready on "+probeAddress(s.ports.PolicyAddr),
			"inspect "+spec.LogPath+" or change POLICY_ADDR")
	}
	return nil
}

func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	children := s.children
	s.children = nil
	s.mu.Unlock()
	var firstErr error
	for i := len(children) - 1; i >= 0; i-- {
		if err := children[i].child.Stop(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("stop %s: %w", children[i].name, err)
		}
	}
	return firstErr
}

func (s *Supervisor) abort(_ context.Context) {
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.Stop(stopCtx)
}

func (s *Supervisor) addChild(name string, child Child) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.children = append(s.children, runningChild{name: name, child: child})
}

func (s *Supervisor) initDatabaseIfNeeded(ctx context.Context, pgdata string, db databaseConfig) error {
	if _, err := os.Stat(filepath.Join(pgdata, "PG_VERSION")); err == nil {
		return nil
	}
	spec := ChildSpec{
		Name:    "initdb",
		Path:    filepath.Join(s.postgresBin, binaryName("initdb")),
		Args:    []string{"-D", pgdata, "-U", db.user, "-A", "trust", "-E", "UTF8"},
		Dir:     s.dataDir,
		Env:     []string{},
		LogPath: filepath.Join(s.dataDir, "logs", "initdb.log"),
	}
	child, err := s.opts.Runner.Start(ctx, spec)
	if err != nil {
		return s.databaseFailure(spec, err)
	}
	if err := child.Wait(); err != nil {
		return s.databaseFailure(spec, err)
	}
	return nil
}

func (s *Supervisor) createDatabase(ctx context.Context, db databaseConfig) error {
	spec := ChildSpec{
		Name:    "createdb",
		Path:    filepath.Join(s.postgresBin, binaryName("createdb")),
		Args:    []string{"-h", db.host, "-p", strconv.Itoa(db.port), "-U", db.user, db.database},
		Dir:     s.dataDir,
		Env:     []string{},
		LogPath: filepath.Join(s.dataDir, "logs", "createdb.log"),
	}
	child, err := s.opts.Runner.Start(ctx, spec)
	if err != nil {
		if databaseAlreadyExists(err) {
			return nil
		}
		return stepFailureCause("start database",
			fmt.Sprintf("database %q could not be created", db.database),
			"inspect "+spec.LogPath+" or run --reset to rebuild the data directory", err)
	}
	if err := child.Wait(); err != nil {
		if databaseAlreadyExists(err) {
			return nil
		}
		return stepFailureCause("start database",
			fmt.Sprintf("database %q could not be created", db.database),
			"inspect "+spec.LogPath+" or run --reset to rebuild the data directory", err)
	}
	return nil
}

func (s *Supervisor) databaseFailure(spec ChildSpec, err error) error {
	if isVCRuntimeFailure(err) {
		return stepFailure("start database",
			"the Visual C++ runtime is missing",
			"install the Visual C++ runtime from "+vcRedistURL)
	}
	return stepFailureCause("start database",
		spec.Name+" failed while preparing the Postgres data directory",
		"inspect "+spec.LogPath+" or run --reset to rebuild the data directory", err)
}

func (s *Supervisor) preparePostgres() (string, error) {
	if s.opts.PostgresBinDir != "" {
		binDir, err := filepath.Abs(s.opts.PostgresBinDir)
		if err != nil {
			return "", fmt.Errorf("prepare demo data directory: cannot resolve the Postgres binary directory %s: %w", s.opts.PostgresBinDir, err)
		}
		info, err := os.Stat(binDir)
		if err != nil || !info.IsDir() {
			return "", fmt.Errorf("prepare demo data directory: Postgres binary directory %s is missing: set PostgresBinDir to a directory containing initdb and postgres", binDir)
		}
		return binDir, nil
	}
	binDir := filepath.Join(s.dataDir, "postgres")
	if initdb, err := findBinary(binDir, "initdb"); err == nil {
		return filepath.Dir(initdb), nil
	}
	archive := s.opts.PostgresArchive
	if archive == "" {
		exe, err := s.executablePath()
		if err != nil {
			return "", fmt.Errorf("prepare demo data directory: cannot locate the running executable: %w", err)
		}
		archive = filepath.Join(filepath.Dir(exe), "postgres.zip")
	}
	if _, err := os.Stat(archive); err != nil {
		return "", fmt.Errorf("prepare demo data directory: Postgres archive %s is missing: place the archive next to the running executable or set PostgresBinDir", archive)
	}
	if err := extractZip(archive, binDir); err != nil {
		return "", fmt.Errorf("prepare demo data directory: cannot extract %s: %w", archive, err)
	}
	initdb, err := findBinary(binDir, "initdb")
	if err != nil {
		return "", fmt.Errorf("prepare demo data directory: extracted archive %s does not contain %s: use a complete Postgres archive or set PostgresBinDir", archive, binaryName("initdb"))
	}
	return filepath.Dir(initdb), nil
}

func (s *Supervisor) resolvePolicyPath() (string, error) {
	if s.opts.PolicyPath != "" {
		path, err := filepath.Abs(s.opts.PolicyPath)
		if err != nil {
			return "", fmt.Errorf("start policy: cannot resolve %s: %w", s.opts.PolicyPath, err)
		}
		return path, nil
	}
	exe, err := s.executablePath()
	if err != nil {
		return "", fmt.Errorf("start policy: cannot locate the running executable: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), binaryName("policy-example")), nil
}

func (s *Supervisor) executablePath() (string, error) {
	if s.opts.ExecutablePath != "" {
		return filepath.Abs(s.opts.ExecutablePath)
	}
	return os.Executable()
}

func resolvePorts(opts Options) (Ports, error) {
	httpAddr := opts.HTTPAddr
	if httpAddr == "" {
		httpAddr = os.Getenv("HTTP_ADDR")
	}
	if httpAddr == "" {
		httpAddr = defaultHTTPAddr
	}
	databaseURL := opts.DatabaseURL
	if databaseURL == "" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	if databaseURL == "" {
		databaseURL = DefaultDatabaseURL
	}
	if _, err := parseDatabaseURL(databaseURL); err != nil {
		return Ports{}, fmt.Errorf("DATABASE_URL: %w", err)
	}
	policyAddr := opts.PolicyAddr
	if policyAddr == "" {
		policyAddr = os.Getenv("POLICY_ADDR")
	}
	if policyAddr == "" {
		policyAddr = defaultPolicyAddr
	}
	return Ports{HTTPAddr: httpAddr, DatabaseURL: databaseURL, PolicyAddr: policyAddr}, nil
}

func resolveDataDir(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	if env := os.Getenv("DEMO_DATA_DIR"); env != "" {
		return filepath.Abs(env)
	}
	if base := os.Getenv("LOCALAPPDATA"); base != "" {
		return filepath.Join(base, "LiteAgentPlatform", "demo"), nil
	}
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		return "", errors.New("cannot resolve the demo data directory: set DEMO_DATA_DIR or LOCALAPPDATA")
	}
	return filepath.Join(cache, "LiteAgentPlatform", "demo"), nil
}

func parseDatabaseURL(raw string) (databaseConfig, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return databaseConfig{}, fmt.Errorf("cannot parse %q: %w", raw, err)
	}
	if parsed.Scheme == "" {
		return databaseConfig{}, fmt.Errorf("%q is not a URL", raw)
	}
	port := 5432
	if value := parsed.Port(); value != "" {
		port, err = strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return databaseConfig{}, fmt.Errorf("invalid port %q", value)
		}
	}
	host := parsed.Hostname()
	if host == "" {
		host = "127.0.0.1"
	}
	user := "postgres"
	if parsed.User != nil {
		if name := parsed.User.Username(); name != "" {
			user = name
		}
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	if database == "" {
		database = "postgres"
	}
	return databaseConfig{host: host, port: port, user: user, database: database}, nil
}

func ensureOutsideRepository(dataDir, repository string) error {
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	absRepo, err := filepath.Abs(repository)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRepo, absData)
	if err != nil {
		return nil
	}
	sep := string(filepath.Separator)
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+sep)) {
		return stepFailure("prepare demo data directory",
			fmt.Sprintf("%s is inside the repository at %s", absData, absRepo),
			"set DEMO_DATA_DIR or Options.DataDir to a directory outside the repository")
	}
	return nil
}

func checkAddressFree(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return listener.Close()
}

func probeAddress(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func tcpProbe(ctx context.Context, spec ChildSpec) error {
	if spec.ReadyAddress == "" {
		return nil
	}
	timeout := spec.ReadyTimeout
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", spec.ReadyAddress, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", spec.ReadyAddress)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func isVCRuntimeFailure(err error) bool {
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"vcruntime", "msvcp", "api-ms-win-crt", "code execution cannot proceed", "0xc0000135", "3221225781"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func databaseAlreadyExists(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "already exists")
}

func stepFailure(step, occupant, remedy string) error {
	return fmt.Errorf("%s: %s: %s", step, occupant, remedy)
}

func stepFailureCause(step, occupant, remedy string, err error) error {
	return fmt.Errorf("%s: %s: %s: %w", step, occupant, remedy, err)
}

func binaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func findBinary(root, name string) (string, error) {
	target := binaryName(name)
	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.EqualFold(entry.Name(), target) && found == "" {
			found = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("%s is not found under %s", target, root)
	}
	return found, nil
}

func extractZip(archive, destination string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	sep := string(filepath.Separator)
	for _, entry := range reader.File {
		cleaned := filepath.Clean(entry.Name)
		if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+sep) {
			return fmt.Errorf("archive entry %q escapes the destination", entry.Name)
		}
		target := filepath.Join(destination, cleaned)
		rel, err := filepath.Rel(destination, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+sep) {
			return fmt.Errorf("archive entry %q escapes the destination", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := extractZipFile(entry, target); err != nil {
			return err
		}
	}
	return nil
}

func extractZipFile(entry *zip.File, target string) error {
	source, err := entry.Open()
	if err != nil {
		return err
	}
	defer source.Close()
	mode := entry.Mode()
	if mode == 0 {
		mode = 0o644
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, source)
	return err
}
