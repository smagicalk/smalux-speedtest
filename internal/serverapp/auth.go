package serverapp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"smalux-speedtest/internal/store"
)

// pageData 是需要 CSRF Token 的管理页面共用模板数据。
type pageData struct {
	// CSRF 被写入 meta 或隐藏表单字段，供管理页面的状态修改请求回传。
	CSRF           string
	CurrentAdminID string
	Username       string
	IsOwner        bool
}

type loginPageData struct {
	Error            string
	Username         string
	RegisterError    string
	RegisterUsername string
	InviteCode       string
	Mode             string
}

type sessionContextKey struct{}

// loginPage 展示登录页；已有有效会话的管理员会被直接送回控制台。
func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := a.session(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_ = a.template.ExecuteTemplate(w, "login.html", loginPageData{Mode: "login"})
}

// registerPage 与登录页共用模板，让已拿到邀请码的新管理员可以自助创建普通账户。
func (a *App) registerPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := a.session(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_ = a.template.ExecuteTemplate(w, "login.html", loginPageData{Mode: "register", InviteCode: strings.TrimSpace(r.URL.Query().Get("invite"))})
}

// login 校验管理员密码并签发 12 小时有效的内存会话。
// Cookie 对 JavaScript 不可见、限制为同站请求，并在 TLS/反向代理 HTTPS 场景开启 Secure。
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = a.template.ExecuteTemplate(w, "login.html", loginPageData{Error: "用户名或密码错误", Mode: "login"})
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
		_ = a.template.ExecuteTemplate(w, "login.html", loginPageData{Error: "用户名或密码错误", Username: username, Mode: "login"})
		return
	}
	session := a.sessions.create(admin.ID, admin.Username, admin.IsOwner)
	setSessionCookie(w, r, session)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// register 使用一次性邀请码创建普通管理员，并直接签发登录会话。
//
// 邀请码是注册资格本身，服务端只保存摘要；失败时对无效、已用和已吊销邀请码使用
// 同一提示，避免公开接口泄漏邀请状态。
func (a *App) register(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 12<<10)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = a.template.ExecuteTemplate(w, "login.html", loginPageData{RegisterError: "注册信息无效", Mode: "register"})
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if password != r.FormValue("password_confirm") {
		w.WriteHeader(http.StatusBadRequest)
		_ = a.template.ExecuteTemplate(w, "login.html", loginPageData{
			RegisterError: "两次输入的密码不一致", RegisterUsername: username, InviteCode: strings.TrimSpace(r.FormValue("invite_code")), Mode: "register",
		})
		return
	}
	admin, err := a.store.RegisterAdminWithInvite(r.Context(), r.FormValue("invite_code"), username, password)
	if err != nil {
		w.WriteHeader(registerErrorStatus(err))
		_ = a.template.ExecuteTemplate(w, "login.html", loginPageData{
			RegisterError: registerErrorMessage(err), RegisterUsername: username, InviteCode: strings.TrimSpace(r.FormValue("invite_code")), Mode: "register",
		})
		return
	}
	session := a.sessions.create(admin.ID, admin.Username, admin.IsOwner)
	setSessionCookie(w, r, session)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func registerErrorStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrAdminUsernameTaken):
		return http.StatusConflict
	case errors.Is(err, store.ErrInvalidAdminUsername), errors.Is(err, store.ErrInvalidAdminPassword), errors.Is(err, store.ErrInvalidAdminInvite):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func registerErrorMessage(err error) string {
	switch {
	case errors.Is(err, store.ErrInvalidAdminUsername):
		return "用户名需为 3-32 位，以字母开头，仅含字母、数字、点、横线或下划线"
	case errors.Is(err, store.ErrInvalidAdminPassword):
		return "密码至少需要 8 个字符、不能超过 72 字节，且不能包含控制字符"
	case errors.Is(err, store.ErrAdminUsernameTaken):
		return "用户名已存在"
	case errors.Is(err, store.ErrInvalidAdminInvite):
		return "邀请码无效或已使用"
	default:
		return "注册失败，请稍后重试"
	}
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

// setSessionCookie is the single policy point for login, registration and password
// change session cookies.
func setSessionCookie(w http.ResponseWriter, r *http.Request, session *session) {
	http.SetCookie(w, &http.Cookie{
		Name: "smalux_session", Value: session.token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: requestIsTLS(r), MaxAge: 12 * 60 * 60,
	})
}

// dashboard 渲染控制台，并把当前会话的 CSRF Token 注入 meta 标签供前端 API 使用。
func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	session, _ := a.session(r)
	_ = a.template.ExecuteTemplate(w, "dashboard.html", pageData{
		CSRF: session.csrf, CurrentAdminID: session.userID, Username: session.username, IsOwner: session.isOwner,
	})
}

// requireOwner 将管理员账户管理等最高权限操作限制给首次引导账户。
func (a *App) requireOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := a.session(r)
		if !ok || !session.isOwner {
			writeError(w, http.StatusForbidden, errors.New("仅最高权限管理员可以执行此操作"))
			return
		}
		next.ServeHTTP(w, r)
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
