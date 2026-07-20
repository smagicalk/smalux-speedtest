package serverapp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// pageData 是需要 CSRF Token 的管理页面共用模板数据。
type pageData struct {
	// CSRF 被写入 meta 或隐藏表单字段，供管理页面的状态修改请求回传。
	CSRF           string
	CurrentAdminID string
	Username       string
}

type loginPageData struct {
	Error    string
	Username string
}

type sessionContextKey struct{}

// loginPage 展示登录页；已有有效会话的管理员会被直接送回控制台。
func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := a.session(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_ = a.template.ExecuteTemplate(w, "login.html", loginPageData{})
}

// login 校验管理员密码并签发 12 小时有效的内存会话。
// Cookie 对 JavaScript 不可见、限制为同站请求，并在 TLS/反向代理 HTTPS 场景开启 Secure。
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = a.template.ExecuteTemplate(w, "login.html", loginPageData{Error: "用户名或密码错误"})
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	if username == "" {
		// 兼容旧版只提交密码的部署脚本；浏览器页面始终显式提交用户名。
		username = "admin"
	}
	admin, err := a.store.AuthenticateAdmin(r.Context(), username, r.FormValue("password"))
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = a.template.ExecuteTemplate(w, "login.html", loginPageData{Error: "用户名或密码错误", Username: username})
		return
	}
	session := a.sessions.create(admin.ID, admin.Username)
	http.SetCookie(w, &http.Cookie{
		Name: "smalux_session", Value: session.token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: requestIsTLS(r), MaxAge: 12 * 60 * 60,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// logout 同时删除服务端会话并通过负 MaxAge 清除浏览器 Cookie。
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if session, ok := a.session(r); ok {
		a.sessions.delete(session.token)
	}
	http.SetCookie(w, &http.Cookie{Name: "smalux_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// dashboard 渲染控制台，并把当前会话的 CSRF Token 注入 meta 标签供前端 API 使用。
func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	session, _ := a.session(r)
	_ = a.template.ExecuteTemplate(w, "dashboard.html", pageData{
		CSRF: session.csrf, CurrentAdminID: session.userID, Username: session.username,
	})
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
		// 页面包含 CSRF Token，API/导出包含测速数据或一次性凭据；认证响应均不可缓存。
		w.Header().Set("Cache-Control", "no-store")
		session, ok := a.session(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			return
		}
		// 后续 CSRF 和业务处理器复用同一份已验证会话，避免一个请求内
		// 重复查询 SQLite 或在两次校验之间出现状态竞态。
		ctx := context.WithValue(r.Context(), sessionContextKey{}, session)
		next.ServeHTTP(w, r.WithContext(ctx))
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

// logRequests 在请求完成后记录稳定路由模板与总耗时。当前日志级别为 Debug，避免正常
// 生产流量占用过多日志空间；它不记录原始 URL、请求体、Cookie 或 Authorization。
// Request.Pattern 由 ServeMux 匹配后填写，只包含注册时的模板，不包含任务 ID 等路径值。
func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		a.config.Logger.Debug("http request", "route", route, "duration", time.Since(start))
	})
}

// session 从 Cookie 读取随机会话标识，并委托 sessionStore 验证存在性和过期时间。
func (a *App) session(r *http.Request) (*session, bool) {
	if value, ok := r.Context().Value(sessionContextKey{}).(*session); ok && value != nil {
		return value, true
	}
	cookie, err := r.Cookie("smalux_session")
	if err != nil {
		return nil, false
	}
	value, ok := a.sessions.get(cookie.Value)
	if !ok {
		return nil, false
	}
	enabled, err := a.store.IsAdminEnabled(r.Context(), value.userID)
	if err != nil || !enabled {
		a.sessions.delete(value.token)
		return nil, false
	}
	return value, true
}
