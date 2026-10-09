// Command dockerfast runs a pull-through caching proxy for Docker registries.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dockerfast/dockerfast/internal/cache"
	"github.com/dockerfast/dockerfast/internal/config"
	"github.com/dockerfast/dockerfast/internal/proxy"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dockerfast:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to JSON config file (optional)")
		listen      = flag.String("listen", "", "listen address, e.g. :5000 (overrides config)")
		dataDir     = flag.String("data-dir", "", "cache data directory (overrides config)")
		logLevel    = flag.String("log-level", "", "log level: debug|info|warn|error (overrides config)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("dockerfast", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	c, err := cache.New(cfg.DataDir, cfg.Cache.MaxSizeBytes, logger)
	if err != nil {
		return err
	}
	reg, err := proxy.New(cfg, c, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go c.StartCleaner(ctx, cfg.Cache.CleanupInterval.Duration(), cfg.Cache.BlobTTL.Duration(), cfg.Cache.ManifestTTL.Duration())

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           reg.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("dockerfast started",
			"version", version,
			"listen", cfg.Listen,
			"dataDir", cfg.DataDir,
			"cacheMaxBytes", cfg.Cache.MaxSizeBytes,
			"upstreams", len(cfg.Upstreams),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("graceful shutdown failed", "error", err)
			_ = srv.Close()
		}
		logger.Info("dockerfast stopped")
		return nil
	}
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}
