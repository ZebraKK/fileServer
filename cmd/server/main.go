package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fileServer/internal/admin"
	"fileServer/internal/cache"
	"fileServer/internal/config"
	"fileServer/internal/domain"
	"fileServer/internal/flush"
	"fileServer/internal/observe"
	"fileServer/internal/origin"
	"fileServer/internal/server"
	"fileServer/internal/storage"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to YAML config file")
	flag.Parse()

	logger := observe.NewLogger()
	slog.SetDefault(logger)

	// ── Config ────────────────────────────────────────────────────────────────
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("failed to load config", slog.String("error", err.Error()))
		os.Exit(1)
	}
	logger.Info("config loaded", slog.String("file", *cfgPath), slog.Int("domains", len(cfg.Domains)))

	// ── Storage ───────────────────────────────────────────────────────────────
	var store storage.Storage
	switch cfg.Storage.Type {
	case "localfs", "":
		store, err = storage.NewLocalFS(cfg.Storage.RootDir)
		if err != nil {
			logger.Error("storage init failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
	default:
		logger.Error("unsupported storage type", slog.String("type", cfg.Storage.Type))
		os.Exit(1)
	}
	logger.Info("storage ready", slog.String("type", cfg.Storage.Type), slog.String("root", cfg.Storage.RootDir))

	// ── Flush rule store ──────────────────────────────────────────────────────
	flushStore := flush.New(store)
	if err := flushStore.Load(); err != nil {
		logger.Error("flush store load failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// ── Cache ─────────────────────────────────────────────────────────────────
	lru := cache.NewLRUCache(cfg.Cache.MaxItems, store)
	sfCache := cache.NewSingleflightCache(lru)

	// ── Key builder ───────────────────────────────────────────────────────────
	keyBuilder := cache.NewKeyBuilder(cfg.KeyRules)

	// ── Origin puller ─────────────────────────────────────────────────────────
	puller := origin.New()

	// ── Domain router ─────────────────────────────────────────────────────────
	deps := domain.Deps{
		Cache:      sfCache,
		FlushStore: flushStore,
		Puller:     puller,
		KeyBuilder: keyBuilder,
	}
	router := domain.New()
	if err := router.Update(cfg.Domains, deps); err != nil {
		logger.Error("domain router init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// ── Admin handler ─────────────────────────────────────────────────────────
	adminHandler := admin.New(sfCache, flushStore, keyBuilder, cfg)

	// ── HTTP servers ──────────────────────────────────────────────────────────
	srvs := server.New(cfg, router, adminHandler, logger)
	srvs.Start()

	// ── Config hot-reload ─────────────────────────────────────────────────────
	go func() {
		if err := config.Watch(*cfgPath, logger, func(newCfg *config.Config) {
			if err := router.Update(newCfg.Domains, deps); err != nil {
				logger.Error("hot-reload: domain router update failed", slog.String("error", err.Error()))
				return
			}
			// Update flush rule cleanup schedule on reload.
			_ = flushStore.Cleanup(newCfg.Cache.FlushRuleMaxAge)
			logger.Info("hot-reload complete", slog.Int("domains", len(newCfg.Domains)))
		}); err != nil {
			logger.Error("config watcher failed", slog.String("error", err.Error()))
		}
	}()

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	logger.Info("shutdown initiated", slog.String("signal", sig.String()))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srvs.Shutdown(ctx)
	logger.Info("shutdown complete")
}
