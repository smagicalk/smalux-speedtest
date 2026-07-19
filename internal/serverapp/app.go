package serverapp

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"smalux-speedtest/internal/importer"
	"smalux-speedtest/internal/model"
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
	// server 是实际提供路由的标准库 HTTP Server。
	server *http.Server
}

// pageData 是需要 CSRF Token 的管理页面共用模板数据。
type pageData struct {
	// CSRF 被写入 meta 或隐藏表单字段，供管理页面的状态修改请求回传。
	CSRF string
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
		Addr:    config.Listen,
		Handler: app.routes(),
		// 限制请求头读取时间和大小，降低慢速请求与异常大请求头占用资源的风险。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return app, nil
}

// ListenAndServe 启动 HTTP 监听并阻塞到服务关闭或发生监听错误。
func (a *App) ListenAndServe() error {
	a.config.Logger.Info("server listening", "address", a.config.Listen, "database", a.config.DatabasePath)
	return a.server.ListenAndServe()
}

// Shutdown 先停止接收 HTTP 请求、等待在途请求结束，再关闭 SQLite 连接。
// HTTP 关闭错误优先返回，否则返回数据库关闭错误。
func (a *App) Shutdown(ctx context.Context) error {
	err := a.server.Shutdown(ctx)
	storeErr := a.store.Close()
	if err != nil {
		return err
	}
	return storeErr
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
	mux.Handle("GET /api/tasks", a.requireAdmin(http.HandlerFunc(a.listTasks)))
	mux.Handle("POST /api/tasks", a.requireAdmin(a.csrf(http.HandlerFunc(a.createTask))))
	mux.Handle("GET /api/tasks/{id}", a.requireAdmin(http.HandlerFunc(a.getTask)))
	mux.Handle("POST /api/tasks/{id}/cancel", a.requireAdmin(a.csrf(http.HandlerFunc(a.cancelTask))))
	mux.Handle("GET /api/tasks/{id}/events", a.requireAdmin(http.HandlerFunc(a.taskEvents)))
	mux.Handle("GET /api/tasks/{id}/results.csv", a.requireAdmin(http.HandlerFunc(a.resultsCSV)))
	return a.logRequests(mux)
}

// loginPage 展示登录页；已有有效会话的管理员会被直接送回控制台。
func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.session(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_ = a.template.ExecuteTemplate(w, "login.html", nil)
}

// login 校验管理员密码并签发 12 小时有效的内存会话。
// Cookie 对 JavaScript 不可见、限制为同站请求，并在 TLS/反向代理 HTTPS 场景开启 Secure。
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !a.store.VerifyAdmin(r.Context(), r.FormValue("password")) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = a.template.ExecuteTemplate(w, "login.html", map[string]string{"Error": "密码错误"})
		return
	}
	session := a.sessions.create()
	http.SetCookie(w, &http.Cookie{
		Name: "smalux_session", Value: session.token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: requestIsTLS(r), MaxAge: 12 * 60 * 60,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// logout 同时删除服务端会话并通过负 MaxAge 清除浏览器 Cookie。
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if session, ok := a.session(r); ok {
		a.sessions.delete(session.token)
	}
	http.SetCookie(w, &http.Cookie{Name: "smalux_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// dashboard 渲染控制台，并把当前会话的 CSRF Token 注入 meta 标签供前端 API 使用。
func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	session, _ := a.session(r)
	_ = a.template.ExecuteTemplate(w, "dashboard.html", pageData{CSRF: session.csrf})
}

// taskPage 渲染单个任务详情页；任务数据和后续增量事件由前端 API/SSE 获取。
func (a *App) taskPage(w http.ResponseWriter, r *http.Request) {
	session, _ := a.session(r)
	_ = a.template.ExecuteTemplate(w, "task.html", struct {
		CSRF   string
		TaskID string
	}{CSRF: session.csrf, TaskID: r.PathValue("id")})
}

// listClients 返回持久化 Client 信息，并合并 Hub 当前进程观察到的在线状态。
// Online 是瞬时运行态，不写入数据库；LastSeen 等历史信息仍由 store 提供。
func (a *App) listClients(w http.ResponseWriter, r *http.Request) {
	clients, err := a.store.ListClients(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	type clientView struct {
		store.Client
		Online bool `json:"online"`
	}
	response := make([]clientView, 0, len(clients))
	for _, client := range clients {
		response = append(response, clientView{Client: client, Online: a.hub.Online(client.ID)})
	}
	writeJSON(w, http.StatusOK, response)
}

// createClient 创建一个可连接的测速节点，并且仅在本次响应中返回明文 Token。
// store 负责以不可逆形式保存认证材料，调用方应妥善保管返回值。
func (a *App) createClient(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	client, token, err := a.store.CreateClient(r.Context(), strings.TrimSpace(input.Name), input.Labels)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"client": client, "token": token})
}

// revokeClient 禁用持久化凭据，并通知 Hub 断开当前连接、终止该 Client 的活动目标。
func (a *App) revokeClient(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.store.RevokeClient(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	a.hub.RevokeClient(r.Context(), id)
	w.WriteHeader(http.StatusNoContent)
}

// listTasks 返回最近 100 个任务摘要，避免管理页面一次读取无限历史记录。
func (a *App) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := a.store.ListTasks(r.Context(), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

// createTask 导入代理、校验测速参数与目标 Client，然后创建并下发一个测速任务。
//
// 持久化边界需要特别注意：store.CreateTask 只保存任务计数、参数和目标 Client 状态；
// parsed.Proxies 中可能含密码、UUID、私钥等字段，只进入随后构造的 Assignment，并由 Hub
// 保存在运行内存中。任务结束或进程退出后不会从数据库恢复这些完整代理配置。
func (a *App) createTask(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Source          string   `json:"source"`
		SubscriptionURL string   `json:"subscription_url"`
		ClientIDs       []string `json:"client_ids"`
		CandidateCount  int      `json:"candidate_count"`
		TopN            int      `json:"top_n"`
		Threads         int      `json:"threads"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(input.ClientIDs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("select at least one client"))
		return
	}
	// 远程订阅与直接粘贴内容可同时存在，两者合并后走同一套格式/base64 解析逻辑。
	// Fetcher 负责 URL 和响应体安全限制，避免处理器直接发起不受约束的服务器端请求。
	if input.SubscriptionURL != "" {
		content, err := a.fetcher.Fetch(r.Context(), input.SubscriptionURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if input.Source != "" {
			input.Source += "\n"
		}
		input.Source += content
	}
	// Parse 会尽可能解析每一条代理并同时收集逐条错误。只要至少有一个代理有效，任务
	// 仍可创建，解析错误则随响应返回供管理员修正无效条目。
	parsed := importer.Parse(input.Source)
	if len(parsed.Proxies) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "没有可测速的代理", "import_errors": parsed.Errors})
		return
	}
	// 0 表示前端未指定参数，应用服务端默认值；显式越界值不会被静默纠正。
	if input.CandidateCount == 0 {
		input.CandidateCount = 10
	}
	if input.TopN == 0 {
		input.TopN = 3
	}
	if input.Threads == 0 {
		input.Threads = 4
	}
	if input.CandidateCount < 1 || input.CandidateCount > 50 || input.TopN < 1 || input.TopN > 3 || input.TopN > input.CandidateCount || input.Threads < 1 || input.Threads > 32 {
		writeError(w, http.StatusBadRequest, errors.New("candidate_count must be 1-50, top_n must be 1-3, and threads must be 1-32"))
		return
	}
	clients, err := a.store.ListClients(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 从数据库快照验证目标是否存在且未撤销。是否在线不影响创建：离线 Client 在本进程
	// 内重连后，Hub 会继续派发处于 queued 的任务。
	validClients := make(map[string]bool)
	for _, client := range clients {
		validClients[client.ID] = client.Enabled
	}
	// 保留提交顺序并去重，确保目标计数、状态聚合和实际下发集合一致。
	uniqueIDs := make([]string, 0, len(input.ClientIDs))
	seen := make(map[string]bool)
	for _, id := range input.ClientIDs {
		if !validClients[id] {
			writeError(w, http.StatusBadRequest, fmt.Errorf("client %s does not exist or is revoked", id))
			return
		}
		if !seen[id] {
			seen[id] = true
			uniqueIDs = append(uniqueIDs, id)
		}
	}
	// 先持久化不含代理凭据的任务骨架，再把完整 Assignment 注册到 Hub。这样 Hub 中的
	// 运行态始终有对应数据库记录；若 AddTask 后服务崩溃，未完成任务不会泄露代理配置。
	taskID := model.NewID()
	task := store.Task{
		ID: taskID, Status: "queued", CandidateCount: input.CandidateCount, TopN: input.TopN, Threads: input.Threads,
		ProxyCount: len(parsed.Proxies), ClientCount: len(uniqueIDs), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := a.store.CreateTask(r.Context(), task, uniqueIDs); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	assignment := model.Assignment{
		TaskID: taskID, Proxies: parsed.Proxies, CandidateCount: input.CandidateCount, TopN: input.TopN, Threads: input.Threads, TimeoutSeconds: 600,
	}
	a.hub.AddTask(assignment, uniqueIDs)
	writeJSON(w, http.StatusCreated, map[string]any{"task": task, "import_errors": parsed.Errors})
}

// getTask 返回一个任务摘要及其所有已持久化测速结果，用于详情页首次加载和手动刷新。
func (a *App) getTask(w http.ResponseWriter, r *http.Request) {
	task, err := a.store.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	results, err := a.store.ListResults(r.Context(), task.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": task, "results": results})
}

// cancelTask 只允许取消 Hub 中仍活跃的任务。Hub 会向在线目标发送取消消息、更新数据库
// 状态并广播 SSE；已经聚合为终态的任务不再留在 Hub，因此返回 409 Conflict。
func (a *App) cancelTask(w http.ResponseWriter, r *http.Request) {
	if err := a.hub.CancelTask(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

// taskEvents 为一个任务建立 Server-Sent Events 长连接。
//
// 连接建立时仅发送注释帧，不重放历史事件；前端应先通过 getTask 获取快照，再用本接口
// 接收进度、结果和状态增量。15 秒 keepalive 注释用于穿过反向代理的空闲连接回收机制，
// X-Accel-Buffering 则请求 Nginx 等代理不要缓存事件流。
func (a *App) taskEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	channel, unsubscribe := a.hub.Subscribe(r.PathValue("id"))
	// 无论浏览器正常关闭、网络断开还是服务端取消 Context，都必须注销订阅以免泄漏。
	defer unsubscribe()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case event := <-channel:
			fmt.Fprintf(w, "data: %s\n\n", eventJSON(event))
			flusher.Flush()
		case <-keepAlive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// resultsCSV 以 UTF-8 CSV 导出任务的持久化结果。
// 开头写入 BOM 以改善常见表格软件对中文 UTF-8 文件的识别；用户可控文本字段还会经
// csvCell 处理，防止以公式前缀开头的值在表格软件中被执行。
func (a *App) resultsCSV(w http.ResponseWriter, r *http.Request) {
	results, err := a.store.ListResults(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="speedtest-results.csv"`)
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	writer := csv.NewWriter(w)
	_ = writer.Write([]string{"client_id", "proxy", "protocol", "address", "server_id", "server", "latency_ms", "jitter_ms", "download_mbps", "upload_mbps", "error", "created_at"})
	for _, result := range results {
		_ = writer.Write([]string{
			csvCell(result.ClientID), csvCell(result.ProxyName), result.Protocol, result.MaskedAddress, result.SpeedServerID,
			csvCell(result.SpeedServerName), formatFloat(result.LatencyMS), formatFloat(result.JitterMS),
			formatFloat(result.DownloadBPS / 1_000_000), formatFloat(result.UploadBPS / 1_000_000), csvCell(result.Error), result.CreatedAt,
		})
	}
	writer.Flush()
}

// requireAdmin 验证管理端 Session Cookie。
// API 未认证时返回机器可读的 401 JSON，页面请求则重定向到登录页。
func (a *App) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.session(r); !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrf 对修改状态的管理请求执行同步 Token 校验。
// Token 优先从 X-CSRF-Token 请求头获取，兼容普通表单的 csrf 字段；只持有 Cookie 而
// 不知道会话内随机 Token 的跨站请求无法通过。GET 等只读接口不套用此中间件。
func (a *App) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := a.session(r)
		provided := r.Header.Get("X-CSRF-Token")
		if provided == "" {
			_ = r.ParseForm()
			provided = r.FormValue("csrf")
		}
		if !ok || provided == "" || provided != session.csrf {
			writeError(w, http.StatusForbidden, errors.New("invalid CSRF token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests 在请求完成后记录方法、路径与总耗时。当前日志级别为 Debug，避免正常
// 生产流量占用过多日志空间；它不记录请求体、Cookie 或 Authorization 等敏感内容。
func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		a.config.Logger.Debug("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}

// session 从 Cookie 读取随机会话标识，并委托 sessionStore 验证存在性和过期时间。
func (a *App) session(r *http.Request) (*session, bool) {
	cookie, err := r.Cookie("smalux_session")
	if err != nil {
		return nil, false
	}
	return a.sessions.get(cookie.Value)
}

// session 表示单个管理员登录会话。token 用于 Cookie 查找，csrf 用于状态修改请求的
// 二次校验；两者是相互独立的密码学随机值。
type session struct {
	// token 是写入 HttpOnly Cookie、用于查找会话的随机值。
	token string
	// csrf 是独立于 Cookie 的随机值，状态修改请求必须显式回传。
	csrf string
	// expires 是服务端判定会话失效的绝对时间。
	expires time.Time
}

// sessionStore 是进程内管理员会话存储。
// 所有访问都由 mu 串行化，因此多个 HTTP handler 可安全并发登录、校验和退出。会话不
// 进入 SQLite，服务重启会主动让所有管理员重新认证，也避免数据库保存可复用 Session。
type sessionStore struct {
	// mu 串行化会话 map 的创建、查询、惰性过期删除和退出删除。
	mu sync.Mutex
	// sessions 以 Cookie token 为键；内容只存在于当前服务端进程内存。
	sessions map[string]*session
}

// newSessionStore 创建空会话表。
func newSessionStore() *sessionStore { return &sessionStore{sessions: make(map[string]*session)} }

// create 生成一对独立随机 token/csrf 值，并登记一个 12 小时有效的会话。
func (s *sessionStore) create() *session {
	value := &session{token: secureToken(), csrf: secureToken(), expires: time.Now().Add(12 * time.Hour)}
	s.mu.Lock()
	s.sessions[value.token] = value
	s.mu.Unlock()
	return value
}

// get 查找并验证会话。过期会话在访问时惰性删除；返回的 session 创建后不再修改，
// 因而释放锁后只读其字段是安全的。
func (s *sessionStore) get(token string) (*session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[token]
	if !ok || time.Now().After(value.expires) {
		delete(s.sessions, token)
		return nil, false
	}
	return value, true
}

// delete 使给定会话立即失效；删除不存在的 token 是幂等操作。
func (s *sessionStore) delete(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// secureToken 返回 32 字节密码学随机数的无填充 URL-safe Base64 表示，适合 Cookie 和
// HTTP Header。rand.Read 在受支持系统上正常不会失败；此处沿用当前接口的无错误返回约定。
func secureToken() string {
	value := make([]byte, 32)
	_, _ = rand.Read(value)
	return base64.RawURLEncoding.EncodeToString(value)
}

// decodeJSON 在 6 MiB 上限内解码单个 JSON 请求体，并拒绝未知字段。
// 限制体积可控制内存占用，拒绝未知字段则能尽早暴露前后端字段拼写或版本不一致。
func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 6<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// writeJSON 写入统一 JSON Content-Type、状态码和响应体。
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeError 使用统一 {"error":"..."} 结构返回错误，便于前端和 API 调用方处理。
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// requestIsTLS 判断客户端原始请求是否使用 HTTPS。
// X-Forwarded-Proto 用于服务部署在可信 TLS 终止反向代理之后的场景；部署方必须覆盖
// 外部传入的同名 Header，防止不可信客户端伪造代理信息。
func requestIsTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// csvCell 阻止用户可控文本被 Excel 等表格软件解释成公式。
// 在危险前缀前添加单引号只影响展示/解释方式，不会执行公式内容。
func csvCell(value string) string {
	if value != "" && strings.ContainsRune("=+-@", rune(value[0])) {
		return "'" + value
	}
	return value
}

// formatFloat 以固定两位小数输出测速数值，保证 CSV 格式稳定且便于比较。
func formatFloat(value float64) string { return strconv.FormatFloat(value, 'f', 2, 64) }
