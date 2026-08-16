// Package web serves the operator UI: a batch submission form and a job table
// that polls itself, rendered server-side and driven by htmx.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/berejant/movie-torrent-finder/internal/config"
	"github.com/berejant/movie-torrent-finder/internal/media"
	"github.com/berejant/movie-torrent-finder/internal/storage"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Notifier is the worker pool's wake-up channel, kept as an interface so the
// web layer does not depend on the pool itself.
type Notifier interface {
	Notify()
}

// Server owns the HTTP side of the application.
type Server struct {
	echo     *echo.Echo
	store    *storage.Store
	cfg      config.Config
	notifier Notifier
	logger   *slog.Logger
	location *time.Location
}

// New wires routes, middleware and templates.
func New(store *storage.Store, cfg config.Config, notifier Notifier, logger *slog.Logger) (*Server, error) {
	location, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return nil, fmt.Errorf("web: load timezone %q: %w", cfg.TZ, err)
	}

	server := &Server{
		echo:     echo.New(),
		store:    store,
		cfg:      cfg,
		notifier: notifier,
		logger:   logger.With("component", "web"),
		location: location,
	}

	server.echo.HideBanner = true
	server.echo.HidePort = true

	renderer, err := newRenderer(server.templateFuncs())
	if err != nil {
		return nil, err
	}
	server.echo.Renderer = renderer

	server.registerMiddleware()
	server.registerRoutes()

	return server, nil
}

func (s *Server) registerMiddleware() {
	s.echo.Use(middleware.Recover())
	s.echo.Use(middleware.RequestID())

	// One structured line per request, with the request id so a UI action can
	// be traced into the worker logs.
	s.echo.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		// Health probes fire constantly (Docker/Synology) and would drown the log.
		Skipper: func(c echo.Context) bool {
			return strings.HasPrefix(c.Path(), "/health/")
		},
		LogStatus:    true,
		LogURI:       true,
		LogMethod:    true,
		LogLatency:   true,
		LogRequestID: true,
		LogError:     true,
		HandleError:  true,
		LogValuesFunc: func(_ echo.Context, v middleware.RequestLoggerValues) error {
			level := slog.LevelInfo
			if v.Status >= http.StatusInternalServerError || v.Error != nil {
				level = slog.LevelError
			}
			s.logger.Log(context.Background(), level, "http request",
				"method", v.Method,
				"uri", v.URI,
				"status", v.Status,
				"latency_ms", v.Latency.Milliseconds(),
				"request_id", v.RequestID,
				"err", v.Error,
			)
			return nil
		},
	}))

	if s.cfg.AuthEnabled() {
		s.echo.Use(middleware.BasicAuthWithConfig(middleware.BasicAuthConfig{
			// Health endpoints stay open so Docker and Synology can probe them.
			Skipper: func(c echo.Context) bool {
				return strings.HasPrefix(c.Path(), "/health/")
			},
			Validator: func(user, password string, _ echo.Context) (bool, error) {
				okUser := subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.AuthUser)) == 1
				okPass := subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.AuthPassword)) == 1
				return okUser && okPass, nil
			},
		}))
		s.logger.Info("basic auth enabled")
	} else {
		s.logger.Warn("basic auth disabled: AUTH_USER/AUTH_PASSWORD not set")
	}
}

func (s *Server) registerRoutes() {
	assets, err := fs.Sub(staticFS, "static")
	if err != nil {
		// The FS is embedded at build time; a failure here is a programming bug.
		panic(fmt.Sprintf("web: sub static fs: %v", err))
	}
	s.echo.StaticFS("/static", assets)

	s.echo.GET("/", s.handleIndex)
	s.echo.GET("/requests/table", s.handleTable)
	s.echo.POST("/requests", s.handleCreateBatch)
	s.echo.POST("/requests/batch", s.handleBatchAction)
	s.echo.POST("/requests/:id/retry", s.handleRetry)
	s.echo.POST("/requests/:id/cancel", s.handleCancel)
	s.echo.POST("/requests/:id/delete", s.handleDelete)

	s.echo.GET("/health/live", s.handleLive)
	s.echo.GET("/health/ready", s.handleReady)
}

// Start runs the HTTP server until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	address := fmt.Sprintf(":%d", s.cfg.HTTPPort)
	s.logger.Info("http server listening", "address", address)

	errs := make(chan error, 1)
	go func() {
		if err := s.echo.Start(address); err != nil && err != http.ErrServerClosed {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.logger.Info("http server shutting down")
		return s.echo.Shutdown(shutdownCtx)
	}
}

// handleLive answers as long as the process is running.
func (s *Server) handleLive(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady checks the two things the service cannot work without: the
// database and a writable output directory. Tracker reachability is
// deliberately excluded, so a tracker outage never restarts the container.
func (s *Server) handleReady(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{"database": "ok", "torrent_files_dir": "ok"}
	ready := true

	if err := s.store.Ping(ctx); err != nil {
		checks["database"] = err.Error()
		ready = false
	}
	if err := checkWritable(s.cfg.TorrentFilesDir); err != nil {
		checks["torrent_files_dir"] = err.Error()
		ready = false
	}

	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "not ready"
	}

	return c.JSON(status, map[string]any{"status": state, "checks": checks})
}

// checkWritable verifies the output directory exists and accepts writes, which
// is the failure Synology volume permissions produce most often.
func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create: %w", err)
	}

	probe, err := os.CreateTemp(dir, ".readiness-*")
	if err != nil {
		return fmt.Errorf("not writable: %w", err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)

	return nil
}

// renderer executes the embedded templates.
type renderer struct {
	templates *template.Template
}

func newRenderer(funcs template.FuncMap) (*renderer, error) {
	parsed, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	return &renderer{templates: parsed}, nil
}

func (r *renderer) Render(w io.Writer, name string, data any, _ echo.Context) error {
	return r.templates.ExecuteTemplate(w, name, data)
}

func (s *Server) templateFuncs() template.FuncMap {
	return template.FuncMap{
		"humanSize": media.HumanSize,
		"localTime": func(t time.Time) string {
			return t.In(s.location).Format("2006-01-02 15:04:05")
		},
		"statusClass": func(status storage.Status) string {
			switch status {
			case storage.StatusDownloaded:
				return "ok"
			case storage.StatusFailed, storage.StatusNotFound:
				return "bad"
			case storage.StatusDuplicate, storage.StatusCancelled:
				return "muted"
			default:
				return "busy"
			}
		},
		"joinNames":    func(names []string) string { return strings.Join(names, ", ") },
		"canCancel":    func(status storage.Status) bool { return status.Cancellable() },
		"canRetry":     func(status storage.Status) bool { return status.Retryable() },
		"baseName":     filepath.Base,
		"statusValues": statusValues,
	}
}

func statusValues() []storage.Status {
	return []storage.Status{
		storage.StatusQueued,
		storage.StatusSearching,
		storage.StatusFound,
		storage.StatusDownloaded,
		storage.StatusNotFound,
		storage.StatusFailed,
		storage.StatusDuplicate,
		storage.StatusCancelled,
	}
}
