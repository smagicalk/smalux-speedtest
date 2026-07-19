package serverapp

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// pageData 是需要 CSRF Token 的管理页面共用模板数据。
type pageData struct {
	// CSRF 被写入 meta 或隐藏表单字段，供管理页面的状态修改请求回传。
	CSRF string
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
