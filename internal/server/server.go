package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"fileServer/internal/admin"
	"fileServer/internal/config"
	"fileServer/internal/domain"
	"fileServer/internal/observe"
)

// Servers bundles the business and admin HTTP servers.
type Servers struct {
	biz   *http.Server
	adm   *http.Server
	logger *slog.Logger
}

// New constructs both HTTP servers and wires routes.
func New(cfg *config.Config, handler *domain.Handler, adminHandler *admin.Handler, logger *slog.Logger) *Servers {
	// ── Business server ───────────────────────────────────────────────────────
	bizR := chi.NewRouter()
	bizR.Use(
		chimw.RequestID,
		Recovery,
		RequestLogger,
	)
	bizR.Mount("/", handler)

	// ── Admin server ──────────────────────────────────────────────────────────
	admR := chi.NewRouter()
	admR.Handle("/metrics", promhttp.Handler())
	admR.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	adminHandler.Register(admR)

	return &Servers{
		biz:    &http.Server{Addr: cfg.Server.Addr, Handler: bizR},
		adm:    &http.Server{Addr: cfg.Admin.Addr, Handler: admR},
		logger: logger,
	}
}

// Start launches both servers in separate goroutines.
// It returns immediately; call Shutdown to stop them.
func (s *Servers) Start() {
	go func() {
		s.logger.Info("business server listening", slog.String("addr", s.biz.Addr))
		if err := s.biz.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			observe.FromContext(context.Background()).Error("biz server error", slog.String("error", err.Error()))
		}
	}()
	go func() {
		s.logger.Info("admin server listening", slog.String("addr", s.adm.Addr))
		if err := s.adm.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			observe.FromContext(context.Background()).Error("admin server error", slog.String("error", err.Error()))
		}
	}()
}

// Shutdown gracefully stops both servers within ctx.
func (s *Servers) Shutdown(ctx context.Context) {
	if err := s.biz.Shutdown(ctx); err != nil {
		s.logger.Error("biz shutdown error", slog.String("error", err.Error()))
	}
	if err := s.adm.Shutdown(ctx); err != nil {
		s.logger.Error("admin shutdown error", slog.String("error", err.Error()))
	}
	s.logger.Info("servers stopped")
}
