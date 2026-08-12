// Command server runs the movie torrent finder: an HTTP UI for scheduling
// tracker searches and a worker pool that saves the resulting .torrent files.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/berejant/movie-torrent-finder/internal/config"
	"github.com/berejant/movie-torrent-finder/internal/jellyfin"
	"github.com/berejant/movie-torrent-finder/internal/storage"
	"github.com/berejant/movie-torrent-finder/internal/tracker"
	"github.com/berejant/movie-torrent-finder/internal/trakt"
	"github.com/berejant/movie-torrent-finder/internal/web"
	"github.com/berejant/movie-torrent-finder/internal/worker"
)

func main() {
	if err := run(); err != nil {
		// Configuration problems arrive here before the logger exists, so the
		// message goes to stderr in plain text where Docker logs will show it.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	slog.SetDefault(logger)
	logger.Info("starting movie torrent finder", "config", cfg)

	// The output directory must exist and be writable before anything else:
	// on Synology this is where a wrong PUID/PGID shows up.
	if err := os.MkdirAll(cfg.TorrentFilesDir, 0o755); err != nil {
		return fmt.Errorf("create TORRENT_FILES_DIR %s: %w", cfg.TorrentFilesDir, err)
	}
	if err := ensureParentDir(cfg.DBPath); err != nil {
		return err
	}

	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	clients := make([]*tracker.Client, 0, len(cfg.Trackers))
	for _, trackerCfg := range cfg.Trackers {
		client, err := tracker.New(trackerCfg, logger)
		if err != nil {
			return err
		}
		clients = append(clients, client)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool := worker.New(store, clients, cfg, logger)
	if err := pool.Recover(ctx); err != nil {
		return err
	}
	pool.Start(ctx)

	// The trakt watchlist is a second source of requests alongside the web form.
	// It is optional, and a failure to reach trakt must never stop the UI, so it
	// only ever logs.
	var syncer *trakt.Syncer
	if cfg.Trakt.Enabled {
		client, err := trakt.NewClient(cfg.Trakt, logger)
		if err != nil {
			return err
		}

		// The token lives in the jellyfin trakt plugin's configuration, which
		// this service reads over the API and writes back to when it refreshes.
		media, err := jellyfin.NewClient(cfg.Jellyfin, logger)
		if err != nil {
			return err
		}
		tokens := trakt.NewTokenSource(media, trakt.NewOAuth(cfg.Trakt), cfg.Jellyfin.UserID, logger)

		syncer = trakt.NewSyncer(store, client, tokens, cfg, pool, logger)
		syncer.Start(ctx)
	} else {
		logger.Info("trakt watchlist sync disabled: TRAKT_ENABLED is not set")
	}

	server, err := web.New(store, cfg, pool, logger)
	if err != nil {
		return err
	}

	err = server.Start(ctx)

	// The HTTP server has stopped; let the workers finish their current task.
	logger.Info("waiting for workers to stop")
	pool.Wait()
	syncer.Wait()
	logger.Info("shutdown complete")

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// ensureParentDir creates the directory holding the SQLite file.
func ensureParentDir(path string) error {
	dir := dirOf(path)
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create DB_PATH directory %s: %w", dir, err)
	}
	return nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == os.PathSeparator {
			return path[:i]
		}
	}
	return ""
}
