package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"smalux-speedtest/internal/serverapp"
)

func main() {
	listen := flag.String("listen", env("SMALUX_LISTEN", ":8080"), "HTTP listen address")
	database := flag.String("db", env("SMALUX_DATABASE", "smalux-speedtest.db"), "SQLite database path")
	logLevel := flag.String("log-level", env("SMALUX_LOG_LEVEL", "info"), "debug, info, warn or error")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := serverapp.New(ctx, serverapp.Config{
		Listen: *listen, DatabasePath: *database, AdminPassword: os.Getenv("SMALUX_ADMIN_PASSWORD"), Logger: logger,
	})
	if err != nil {
		logger.Error("initialize server", "error", err)
		os.Exit(1)
	}

	errChannel := make(chan error, 1)
	go func() { errChannel <- application.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := application.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown server", "error", err)
		}
	case err := <-errChannel:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped", "error", err)
			os.Exit(1)
		}
	}
}

func parseLevel(value string) slog.Level {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
