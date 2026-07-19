// smalux-server 是分布式代理测速系统的服务端可执行程序。
//
// 业务 HTTP 路由和 WebSocket 调度位于 internal/serverapp；本包只负责解析启动配置、
// 初始化结构化日志、响应进程信号并执行有超时上限的优雅关闭。
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"smalux-speedtest/internal/serverapp"
)

// main 解析启动配置、创建服务端，并在监听错误或系统终止信号之间协调退出。
// 初始化或非正常监听失败使用退出码 1；正常收到信号时执行有界优雅关闭。
func main() {
	// flag 的默认值来自环境变量，因此配置优先级为“命令行参数 > 环境变量 > 内置值”。
	// 这种方式既便于容器注入环境变量，也允许运维临时用参数覆盖配置。
	listen := flag.String("listen", env("SMALUX_LISTEN", ":8080"), "HTTP listen address")
	database := flag.String("db", env("SMALUX_DATABASE", "smalux-speedtest.db"), "SQLite database path")
	logLevel := flag.String("log-level", env("SMALUX_LOG_LEVEL", "info"), "debug, info, warn or error")
	// Bot Token 刻意只从环境变量读取，避免明文秘密出现在进程命令行和进程列表中。
	telegramOwnerID := flag.Int64("telegram-owner-id", envInt64("SMALUX_TELEGRAM_OWNER_ID", 0), "Telegram owner numeric user ID")
	telegramAPIBaseURL := flag.String("telegram-api-base-url", env("SMALUX_TELEGRAM_API_BASE_URL", ""), "Telegram Bot API base URL (optional)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	// NotifyContext 把 Ctrl+C 与容器/系统服务常用的 SIGTERM 统一转换成 Context 取消。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := serverapp.New(ctx, serverapp.Config{
		Listen: *listen, DatabasePath: *database, AdminPassword: os.Getenv("SMALUX_ADMIN_PASSWORD"), Logger: logger,
		TelegramBotToken: os.Getenv("SMALUX_TELEGRAM_BOT_TOKEN"), TelegramOwnerID: *telegramOwnerID, TelegramAPIBaseURL: *telegramAPIBaseURL,
	})
	if err != nil {
		logger.Error("initialize server", "error", err)
		os.Exit(1)
	}

	// ListenAndServe 是阻塞调用，放入 goroutine 后即可同时等待系统信号和监听错误。
	// channel 带一个缓冲，避免主 goroutine 已进入关闭分支时发送方无法退出。
	errChannel := make(chan error, 1)
	go func() { errChannel <- application.ListenAndServe() }()
	select {
	case <-ctx.Done():
		// 给在途 HTTP 请求最多 10 秒完成；超时后 http.Server 会返回关闭错误。
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

// parseLevel 把用户输入转换为 slog 日志级别。
// 未识别的值安全地回退到 info，避免拼写错误意外开启大量 debug 日志。
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

// env 返回非空环境变量，否则返回 fallback。
// 空字符串按“未配置”处理，使启动参数始终具有可用默认值。
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// envInt64 读取十进制整数环境变量；缺失或格式错误时返回 fallback。
// Telegram 配置启用后，serverapp.New 还会对 Owner ID 做严格正数校验。
func envInt64(key string, fallback int64) int64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
