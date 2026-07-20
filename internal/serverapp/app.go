package serverapp

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/subscription"
)

// webFiles 将管理端模板、脚本和样式编译进服务端二进制，部署时不依赖外部静态目录。
//
//go:embed web/*
var webFiles embed.FS

// Config 描述服务端进程启动所需的外部配置。
type Config struct {
	// Listen 是 http.Server 使用的监听地址，例如 ":8080"。
	Listen string
	// DatabasePath 是 SQLite 数据库文件路径。
	DatabasePath string
	// AdminPassword 只在数据库尚未初始化管理员密码时用于引导创建管理员。
	AdminPassword string
	// TelegramBotToken 是 BotFather 签发的令牌。为空时 Telegram Bot 完全禁用。
	TelegramBotToken string
	// TelegramOwnerID 是启动时指定的唯一最高权限 Telegram 用户数字 ID。
	// 启用 Bot 时必须为正数，数据库中的 owner 会以该值为准幂等同步。
	TelegramOwnerID int64
	// TelegramAPIBaseURL 用于测试或自建 Bot API Server；远程地址必须使用 HTTPS。
	TelegramAPIBaseURL string
	// Logger 接收 HTTP、连接与调度日志；为 nil 时使用 slog.Default。
	Logger *slog.Logger
}

// App 组合服务端各基础设施，并拥有 HTTP Server 和 SQLite Store 的生命周期。
type App struct {
	// config 保留监听地址、数据库路径和日志器等启动配置。
	config Config
	// store 持久化账户、Client、任务元数据和脱敏结果。
	store *store.Store
	// hub 管理 Client WebSocket、敏感任务运行态和 SSE 订阅。
	hub *Hub
	// fetcher 在创建任务时安全抓取远程订阅内容。
	fetcher *subscription.Fetcher
	// template 是从 webFiles 一次性解析的管理页面模板集合。
	template *template.Template
	// sessions 是仅驻留内存的管理员登录会话表；进程重启后全部失效。
	sessions *sessionStore
	// telegram 为可选 Bot 后台任务；未配置 Token 和 Owner ID 时保持 nil。
	telegram *telegramRuntime
	// server 是实际提供路由的标准库 HTTP Server。
	server *http.Server
}

// New 初始化数据库、管理员凭据、模板、Hub 和 HTTP Server。
// 任一步失败都会关闭已经打开的数据库，调用方只有在返回 nil error 后才接管 App 生命周期。
func New(ctx context.Context, config Config) (*App, error) {
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	database, err := store.Open(config.DatabasePath)
	if err != nil {
		return nil, err
	}
	// BootstrapAdmin 只负责首次引导；已存在的密码哈希不会被环境变量静默覆盖。
	if err := database.BootstrapAdmin(ctx, config.AdminPassword); err != nil {
		database.Close()
		return nil, err
	}
	templates, err := template.ParseFS(webFiles, "web/*.html")
	if err != nil {
		database.Close()
		return nil, err
	}
	app := &App{
		config: config, store: database, fetcher: subscription.NewFetcher(), template: templates,
		sessions: newSessionStore(),
	}
	app.hub = NewHub(database, config.Logger)
	app.server = &http.Server{
		Addr:     config.Listen,
		Handler:  app.routes(),
		ErrorLog: newSanitizedHTTPErrorLog(config.Logger),
		// 限制请求头读取时间和大小，降低慢速请求与异常大请求头占用资源的风险。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	if err := app.startTelegram(ctx); err != nil {
		database.Close()
		return nil, err
	}
	return app, nil
}

// ListenAndServe 启动 HTTP 监听并阻塞到服务关闭或发生监听错误。
func (a *App) ListenAndServe() error {
	// 数据库路径和监听 IP 都属于部署拓扑，成功日志只记录稳定事件名称。
	a.config.Logger.Info("server listening")
	return a.server.ListenAndServe()
}

// Shutdown 同时开始停止 Bot 和 HTTP；两者退出后再关闭 Hub WebSocket、
// 终结内存任务，最后关闭 SQLite。任一等待超时都保留数据库连接，避免
// 仍在运行的读循环或定时器访问已关闭存储。
func (a *App) Shutdown(ctx context.Context) error {
	a.cancelTelegram()
	// 先通知 Hub 关闭 WebSocket/SSE，否则 http.Server.Shutdown 会等待长连接
	// 直到超时。数据库和任务此时仍保留，供在途 HTTP/Bot 请求收尾。
	a.hub.BeginShutdown()
	httpErr := a.server.Shutdown(ctx)
	telegramErr := a.waitTelegram(ctx)
	if httpErr != nil {
		return httpErr
	}
	if telegramErr != nil {
		return telegramErr
	}
	if err := a.hub.Shutdown(ctx); err != nil {
		return err
	}
	return a.store.Close()
}

// routes 构建服务端完整路由表，并在最外层统一记录请求日志。
//
// 管理页面和 API 使用内存 Session Cookie；所有会修改状态的管理接口还必须通过 CSRF
// 校验。Client WebSocket 不使用管理员会话，而是在 Hub 中用独立的 Bearer Token 认证。
func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	staticFS, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	// WebSocket 在升级前自行校验 Client Token，因此不套 requireAdmin。
	mux.HandleFunc("GET /ws/client", a.hub.ServeWebSocket)

	mux.Handle("GET /", a.requireAdmin(http.HandlerFunc(a.dashboard)))
	mux.Handle("GET /tasks/{id}", a.requireAdmin(http.HandlerFunc(a.taskPage)))
	mux.Handle("POST /logout", a.requireAdmin(a.csrf(http.HandlerFunc(a.logout))))
	mux.Handle("GET /api/clients", a.requireAdmin(http.HandlerFunc(a.listClients)))
	mux.Handle("POST /api/clients", a.requireAdmin(a.csrf(http.HandlerFunc(a.createClient))))
	mux.Handle("DELETE /api/clients/{id}", a.requireAdmin(a.csrf(http.HandlerFunc(a.revokeClient))))
	mux.Handle("GET /api/admin-users", a.requireAdmin(http.HandlerFunc(a.listAdminUsers)))
	mux.Handle("POST /api/admin-users", a.requireAdmin(a.csrf(http.HandlerFunc(a.createAdminUser))))
	mux.Handle("PATCH /api/admin-users/{id}", a.requireAdmin(a.csrf(http.HandlerFunc(a.updateAdminUser))))
	mux.Handle("DELETE /api/admin-users/{id}", a.requireAdmin(a.csrf(http.HandlerFunc(a.deleteAdminUser))))
	mux.Handle("GET /api/tasks", a.requireAdmin(http.HandlerFunc(a.listTasks)))
	mux.Handle("POST /api/tasks", a.requireAdmin(a.csrf(http.HandlerFunc(a.createTask))))
	mux.Handle("GET /api/tasks/{id}", a.requireAdmin(http.HandlerFunc(a.getTask)))
	mux.Handle("POST /api/tasks/{id}/cancel", a.requireAdmin(a.csrf(http.HandlerFunc(a.cancelTask))))
	mux.Handle("GET /api/tasks/{id}/events", a.requireAdmin(http.HandlerFunc(a.taskEvents)))
	mux.Handle("GET /api/tasks/{id}/results.csv", a.requireAdmin(http.HandlerFunc(a.resultsCSV)))
	return a.logRequests(mux)
}
