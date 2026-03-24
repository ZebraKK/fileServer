package config

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fsnotify/fsnotify"
	"github.com/goccy/go-yaml"
)

// Load reads and parses the YAML config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", path, err)
	}
	cfg.Defaults()
	return &cfg, nil
}

// Watch monitors path for changes (fsnotify) and SIGHUP signals.
// On each change, it reloads the config and calls onChange with the new value.
// If reload fails, it logs the error and keeps the previous config.
// Watch blocks; run it in a goroutine.
func Watch(path string, logger *slog.Logger, onChange func(*Config)) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("config: create watcher: %w", err)
	}
	defer watcher.Close()

	if err := watcher.Add(path); err != nil {
		return fmt.Errorf("config: watch %q: %w", path, err)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP)
	defer signal.Stop(sigs)

	logger.Info("config watcher started", slog.String("file", path))

	reload := func() {
		cfg, err := Load(path)
		if err != nil {
			logger.Error("config reload failed", slog.String("error", err.Error()))
			return
		}
		logger.Info("config reloaded", slog.Int("domains", len(cfg.Domains)))
		onChange(cfg)
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				reload()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			logger.Error("config watcher error", slog.String("error", err.Error()))
		case <-sigs:
			logger.Info("SIGHUP received, reloading config")
			reload()
		}
	}
}
