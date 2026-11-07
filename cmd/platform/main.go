package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeokeita231/Lite-agent-platform/internal/config"
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
	if err := run(); err != nil {
		log.Fatalf("platform: %v", err)
	}
}

func keygen(w io.Writer) error {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	apiKey := hex.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(apiKey))
	payload := struct {
		APIKey       string `json:"api_key"`
		APIKeyHash   string `json:"api_key_hash"`
		APIKeyPrefix string `json:"api_key_prefix"`
	}{
		APIKey:       apiKey,
		APIKeyHash:   hex.EncodeToString(sum[:]),
		APIKeyPrefix: apiKey[:8],
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
	if err := migrate.Run(ctx, pool); err != nil {
		return err
	}
	if err := store.ResetRunningRuns(ctx); err != nil {
		return err
	}

	ready := observability.NewReadiness()
	ready.SetReady(true)
	maint := &dbMaintenance{pool: pool, ready: ready, migrated: true}
	go maint.loop(ctx, maintenanceInterval)

	gateway := modelgw.New(cfg.ModelBaseURL, cfg.ModelAPIKey, cfg.ModelChainBudget)
	tools := toolgw.New(store, cfg.StepTimeout)
	dispatcher := agentruntime.New(store, gateway, tools, agentruntime.Options{
		StepTimeout:     cfg.StepTimeout,
		MaxStepAttempts: cfg.MaxStepAttempts,
		PollInterval:    cfg.PollInterval,
	})
	go dispatcher.Run(ctx)

	server := &http.Server{Addr: cfg.HTTPAddr, Handler: httpapi.NewMux(ready, store, tools, ready)}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ListenAndServe()
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	select {
	case <-signals:
	case err := <-serveErr:
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
	return nil
}

type dbMaintenance struct {
	pool     *pgxpool.Pool
	ready    *observability.Readiness
	migrated bool
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
		d.migrated = true
	}
	d.ready.SetReady(true)
}

func (d *dbMaintenance) loop(ctx context.Context, interval time.Duration) {
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
