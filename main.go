package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func initDB() *pgxpool.Pool {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		slog.Error("DATABASE_URL environment variable is not set")
		os.Exit(1)
	}

	config, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		slog.Error("Unable to parse database config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	config.MaxConns = 25
	config.MinConns = 5
	config.MaxConnIdleTime = 5 * time.Minute
	config.MaxConnLifetime = 1 * time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		slog.Error("Unable to create connection pool", slog.String("error", err.Error()))
		os.Exit(1)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		slog.Error("Unable to establish initial database ping", slog.String("error", err.Error()))
		os.Exit(1)
	}

	return pool
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				reqID := GetRequestID(r.Context())
				slog.Error("panic_recovered",
					slog.String("request_id", reqID),
					slog.String("path", r.URL.Path),
					slog.String("method", r.Method),
					slog.Any("panic", err),
				)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func main() {
	jsonHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slog.SetDefault(slog.New(jsonHandler))

	pool := initDB()
	defer pool.Close()

	srv := &Server{DB: pool}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := srv.DB.Ping(ctx); err != nil {
			reqID := GetRequestID(r.Context())
			slog.Error("health_check_failed",
				slog.String("request_id", reqID),
				slog.String("error", err.Error()),
			)
			http.Error(w, "Database unavailable", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	mux.HandleFunc("POST /rsvp", srv.HandleCreateRSVP)
	mux.HandleFunc("GET /rsvp", srv.HandleListRSVPs)
	mux.HandleFunc("GET /rsvp/stats", srv.HandleRSVPStats)

	// Middleware chain: Recovery -> Tracing (sets ID) -> Logging (logs ID & timing) -> Mux
	handler := recoverMiddleware(TracingMiddleware(LoggingMiddleware(mux)))

	httpServer := &http.Server{
		Addr:         ":8080",
		Handler:      handler,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		slog.Info("server_started", slog.String("addr", ":8080"))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server_listen_error", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	sig := <-stopChan
	slog.Info("server_shutting_down", slog.String("signal", sig.String()))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("server_forced_shutdown", slog.String("error", err.Error()))
		os.Exit(1)
	}

	slog.Info("server_exited_cleanly")
}