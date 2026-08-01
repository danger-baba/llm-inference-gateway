package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/danger-baba/llm-inference-gateway/internal/config"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
	"github.com/danger-baba/llm-inference-gateway/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to the gateway config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.Observability.LogLevel)

	redisClient := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: os.Getenv(cfg.Redis.PasswordEnv),
		DB:       cfg.Redis.DB,
	})
	defer redisClient.Close()

	pgDSN := os.Getenv(cfg.Postgres.DSNEnv)
	if pgDSN == "" {
		return fmt.Errorf("main: environment variable %s (postgres.dsn_env) is not set", cfg.Postgres.DSNEnv)
	}
	pgPool, err := pgxpool.New(context.Background(), pgDSN)
	if err != nil {
		return fmt.Errorf("main: postgres pool: %w", err)
	}
	defer pgPool.Close()

	providerSet, providerTimeouts, err := buildProviders(cfg.Providers)
	if err != nil {
		return err
	}

	srv, err := server.New(server.Options{
		Addr:            cfg.Server.Addr,
		ReadTimeout:     cfg.Server.ReadTimeout.Std(),
		RequestTimeout:  cfg.Server.RequestTimeout.Std(),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
		ShutdownTimeout: cfg.Server.ShutdownTimeout.Std(),
		Redis:           redisPinger{redisClient, cfg.Redis.DialTimeout.Std()},
		Postgres:        postgresPinger{pgPool, cfg.Postgres.PingTimeout.Std()},
		Logger:          logger,
		Router:          router.New(cfg),
		Providers:       providerSet,
		ProviderTimeout: providerTimeouts,
	})
	if err != nil {
		return err
	}

	logger.Info("gateway: listening", "addr", srv.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("main: server: %w", err)
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// redisPinger and postgresPinger adapt the real drivers to server.Pinger,
// applying their own configured timeout so a single stalled dependency
// can't hang /readyz.

type redisPinger struct {
	client  *redis.Client
	timeout time.Duration
}

func (p redisPinger) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	return p.client.Ping(ctx).Err()
}

type postgresPinger struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

func (p postgresPinger) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	return p.pool.Ping(ctx)
}
