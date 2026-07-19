package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"smalux-speedtest/internal/clientapp"
)

var version = "dev"

func main() {
	serverURL := flag.String("server", env("SMALUX_SERVER_URL", "ws://127.0.0.1:8080/ws/client"), "server WebSocket URL")
	token := flag.String("token", os.Getenv("SMALUX_CLIENT_TOKEN"), "client token")
	name := flag.String("name", env("SMALUX_CLIENT_NAME", hostname()), "client name")
	labelsText := flag.String("labels", os.Getenv("SMALUX_CLIENT_LABELS"), "comma separated key=value labels")
	logLevel := flag.String("log-level", env("SMALUX_LOG_LEVEL", "info"), "debug, info, warn or error")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	application, err := clientapp.New(clientapp.Config{
		ServerURL: *serverURL, Token: *token, Name: *name, Version: version, Labels: parseLabels(*labelsText), Logger: logger,
	})
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := application.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("client stopped", "error", err)
		os.Exit(1)
	}
}

func parseLabels(value string) map[string]string {
	labels := make(map[string]string)
	for _, item := range strings.Split(value, ",") {
		parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(parts) == 2 && parts[0] != "" {
			labels[parts[0]] = parts[1]
		}
	}
	return labels
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

func hostname() string {
	value, err := os.Hostname()
	if err != nil || value == "" {
		return "smalux-client"
	}
	return value
}
