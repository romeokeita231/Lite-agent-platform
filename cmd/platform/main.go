package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeokeita231/Lite-agent-platform/internal/config"
	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/httpapi"
	"github.com/romeokeita231/Lite-agent-platform/internal/migrate"
	"github.com/romeokeita231/Lite-agent-platform/internal/modelgw"
	"github.com/romeokeita231/Lite-agent-platform/internal/observability"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	agentruntime "github.com/romeokeita231/Lite-agent-platform/internal/runtime"
	"github.com/romeokeita231/Lite-agent-platform/internal/toolgw"
)

const (
	dbCheckTimeout      = 5 * time.Second
	maintenanceInterval = time.Second
	shutdownGrace       = 5 * time.Second
	httpShutdownTimeout = 10 * time.Second
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		if err := keygen(os.Stdout); err != nil {
			log.Fatalf("platform keygen: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "seed" {
		if err := runSeed(os.Stdout); err != nil {
			log.Fatalf("platform seed: %v", err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatalf("platform: %v", err)
	}
}

func newAPIKey() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func keygen(w io.Writer) error {
	apiKey, err := newAPIKey()
	if err != nil {
		return err
	}
	payload := struct {
		APIKey       string `json:"api_key"`
		APIKeyHash   string `json:"api_key_hash"`
		APIKeyPrefix string `json:"api_key_prefix"`
	}{
		APIKey:       apiKey,
		APIKeyHash:   domain.HashAPIKey(apiKey),
		APIKeyPrefix: domain.APIKeyPrefix(apiKey),
	}
	return json.NewEncoder(w).Encode(payload)
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := repository.New(pool)

	ready := observability.NewReadiness()
	dispatcherStarted := make(chan struct{})
	maint := &dbMaintenance{pool: pool, ready: ready, recoverOnStart: true, started: dispatcherStarted}

	metrics := observability.NewMetrics()
	gateway := modelgw.NewWithStore(store, cfg.ModelBaseURL, cfg.ModelAPIKey, cfg.ModelChainBudget)
	gateway.SetMetrics(metrics)
	tools := toolgw.New(store, cfg.StepTimeout)
	tools.SetMetrics(metrics)
	dispatcher := agentruntime.New(store, gateway, tools, agentruntime.Options{
		StepTimeout:       cfg.StepTimeout,
		MaxStepAttempts:   cfg.MaxStepAttempts,
		PollInterval:      cfg.PollInterval,
		TenantConcurrency: cfg.TenantConcurrency,
		TenantRate:        cfg.TenantRate,
		ContextRecent:     cfg.ContextRecent,
		ContextMaxKeep:    cfg.ContextMaxKeep,
		Metrics:           metrics,
	})
	go func() {
		select {
		case <-dispatcherStarted:
			dispatcher.Run(ctx)
		case <-ctx.Done():
		}
	}()

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", metrics.Handler())
	metricsServer := &http.Server{Addr: cfg.MetricsAddr, Handler: metricsMux}

	server := &http.Server{Addr: cfg.HTTPAddr, Handler: httpapi.NewMux(ready, store, tools, ready, gateway)}

	serveErr := make(chan error, 1)
	metricsErr := make(chan error, 1)
	go func() {
		serveErr <- server.ListenAndServe()
	}()
	go func() {
		metricsErr <- metricsServer.ListenAndServe()
	}()
	go maint.loop(ctx, maintenanceInterval)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	select {
	case <-signals:
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case err := <-metricsErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	ready.SetDraining(true)
	ready.SetReady(false)
	dispatcher.Stop(cfg.StepTimeout + shutdownGrace)
	cancel()
	if err := store.ResetRunningRuns(context.Background()); err != nil {
		log.Printf("shutdown: reset running runs: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown metrics: %v", err)
	}
	return nil
}

type dbMaintenance struct {
	pool           *pgxpool.Pool
	ready          *observability.Readiness
	migrated       bool
	recoverOnStart bool
	started        chan struct{}
	startOne       sync.Once
}

func (d *dbMaintenance) check(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, dbCheckTimeout)
	defer cancel()

	if err := d.pool.Ping(checkCtx); err != nil {
		d.ready.SetReady(false)
		return
	}
	if !d.migrated {
		if err := migrate.Run(checkCtx, d.pool); err != nil {
			d.ready.SetReady(false)
			return
		}
		if d.recoverOnStart {
			if err := repository.New(d.pool).ResetRunningRuns(checkCtx); err != nil {
				d.ready.SetReady(false)
				return
			}
		}
		d.migrated = true
		if d.started != nil {
			d.startOne.Do(func() { close(d.started) })
		}
	}
	d.ready.SetReady(true)
}

func (d *dbMaintenance) loop(ctx context.Context, interval time.Duration) {
	d.check(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.check(ctx)
		}
	}
}
