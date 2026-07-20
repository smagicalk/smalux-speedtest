// smalux-client 是部署在不同网络位置的测速执行端。
//
// 进程通过 WebSocket 与中心服务保持连接，接收包含代理配置的测速任务，
// 再将进度和结果回传给服务端。普通配置可由参数或环境变量提供；Client Token
// 优先从环境变量读取，避免明文凭据出现在进程命令行中。
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
	"smalux-speedtest/internal/logsafe"
)

// version 在开发构建中为 dev，发布构建可通过 -ldflags "-X main.version=..."
// 注入实际版本号。该值会在 WebSocket 握手时上报，便于服务端识别客户端版本。
var version = "dev"

// main 解析运行配置、初始化结构化日志，并把 SIGINT/SIGTERM 转换为 context 取消信号。
// 配置错误使用退出码 2；客户端运行期异常使用退出码 1；正常取消则直接退出。
func main() {
	// 普通 flag 的默认值按“环境变量 -> 内置默认值”的顺序确定。Token 是例外：环境变量
	// 明确优先，-token 只保留为旧部署兼容回退，避免凭据出现在进程列表或 flag 默认值中。
	serverURL := flag.String("server", env("SMALUX_SERVER_URL", "ws://127.0.0.1:8080/ws/client"), "server WebSocket URL")
	tokenFlag := flag.String("token", "", "client token (legacy fallback; SMALUX_CLIENT_TOKEN takes precedence)")
	name := flag.String("name", env("SMALUX_CLIENT_NAME", hostname()), "client name")
	labelsText := flag.String("labels", os.Getenv("SMALUX_CLIENT_LABELS"), "comma separated key=value labels")
	logLevel := flag.String("log-level", env("SMALUX_LOG_LEVEL", "info"), "debug, info, warn or error")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	application, err := clientapp.New(clientapp.Config{
		ServerURL: *serverURL, Token: resolveClientToken(*tokenFlag), Name: *name, Version: version, Labels: parseLabels(*labelsText), Logger: logger,
	})
	if err != nil {
		logger.Error("invalid configuration", "error_type", logsafe.ErrorType(err))
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := application.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("client stopped", "error_type", logsafe.ErrorType(err))
		os.Exit(1)
	}
}

// resolveClientToken gives a non-empty environment value precedence over the legacy
// flag. Keeping the flag default empty also prevents `smalux-client -help` from
// printing a token inherited from the process environment.
func resolveClientToken(flagValue string) string {
	if value := strings.TrimSpace(os.Getenv("SMALUX_CLIENT_TOKEN")); value != "" {
		return value
	}
	return strings.TrimSpace(flagValue)
}

// parseLabels 将逗号分隔的 key=value 列表转换为握手时上报的客户端标签。
// 空项、缺少等号或 key 为空的项会被忽略；重复 key 以后出现的值为准。
// SplitN(..., 2) 保证标签值本身可以继续包含等号。
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

// parseLevel 将用户输入映射到 slog 级别。未知值回退到 info，避免日志配置拼写错误
// 导致进程无法启动。
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

// env 返回非空环境变量；变量未设置或值为空时返回 fallback。
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// hostname 提供默认客户端名称。无法读取主机名时使用稳定的通用名称，保证必填配置
// 不会因为宿主环境缺少 hostname 而为空。
func hostname() string {
	value, err := os.Hostname()
	if err != nil || value == "" {
		return "smalux-client"
	}
	return value
}
