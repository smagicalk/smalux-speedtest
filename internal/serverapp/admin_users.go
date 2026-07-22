package serverapp

import (
	"errors"
	"net/http"
	"strings"

	"smalux-speedtest/internal/store"
)

type createAdminUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type updateAdminUserRequest struct {
	Enabled *bool `json:"enabled"`
}

type createAdminInviteResponse struct {
	Invite store.AdminInvite `json:"invite"`
	Code   string            `json:"code"`
}

// listAdminUsers 返回不含密码哈希的账户元数据。当前账户 ID 已由页面 meta
// 提供，前端用它禁用自删除/自停用操作。
func (a *App) listAdminUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.store.ListAdminUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

// listAdminInvites 返回邀请码状态列表。明文 code 不可恢复，因此不会出现在列表中。
func (a *App) listAdminInvites(w http.ResponseWriter, r *http.Request) {
	invites, err := a.store.ListAdminInvites(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, invites)
}

// createAdminInvite 生成一次性邀请码。code 只在本次 JSON 响应中出现，页面刷新后无法找回。
func (a *App) createAdminInvite(w http.ResponseWriter, r *http.Request) {
	session, _ := a.session(r)
	invite, code, err := a.store.CreateAdminInvite(r.Context(), session.userID)
	if err != nil {
		writeAdminUserError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, createAdminInviteResponse{Invite: invite, Code: code})
}

// revokeAdminInvite 撤销尚未使用的邀请码。已使用的邀请码保留审计状态。
func (a *App) revokeAdminInvite(w http.ResponseWriter, r *http.Request) {
	if err := a.store.RevokeAdminInvite(r.Context(), r.PathValue("id")); err != nil {
		writeAdminUserError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// createAdminUser 创建新的启用账户。请求体单独限制为 8 KiB，避免共用业务
// JSON 上限让小型认证接口承担不必要的内存开销。
func (a *App) createAdminUser(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var request createAdminUserRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("请求格式无效"))
		return
	}
	user, err := a.store.CreateAdminUser(r.Context(), request.Username, request.Password)
	if err != nil {
		writeAdminUserError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

// updateAdminUser 目前只切换启用状态。当前登录账户不能停用自己，避免请求
// 成功后立即失去恢复入口；Store 还会并发安全地保护最后一个启用账户。
func (a *App) updateAdminUser(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminUserID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("管理员不存在"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var request updateAdminUserRequest
	if err := decodeJSON(r, &request); err != nil || request.Enabled == nil {
		writeError(w, http.StatusBadRequest, errors.New("请求格式无效"))
		return
	}
	session, _ := a.session(r)
	if !*request.Enabled && session.userID == adminID {
		writeError(w, http.StatusConflict, errors.New("不能停用当前登录账户"))
		return
	}
	if err := a.store.SetAdminUserEnabled(r.Context(), adminID, *request.Enabled); err != nil {
		writeAdminUserError(w, err)
		return
	}
	if !*request.Enabled {
		a.sessions.deleteUser(adminID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteAdminUser 永久删除非当前账户，并同步撤销它的全部内存会话。
func (a *App) deleteAdminUser(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminUserID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("管理员不存在"))
		return
	}
	session, _ := a.session(r)
	if session.userID == adminID {
		writeError(w, http.StatusConflict, errors.New("不能删除当前登录账户"))
		return
	}
	if err := a.store.DeleteAdminUser(r.Context(), adminID); err != nil {
		writeAdminUserError(w, err)
		return
	}
	a.sessions.deleteUser(adminID)
	w.WriteHeader(http.StatusNoContent)
}

func writeAdminUserError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidAdminUsername):
		writeError(w, http.StatusBadRequest, errors.New("用户名需为 3-32 位，以字母开头，仅含字母、数字、点、横线或下划线"))
	case errors.Is(err, store.ErrInvalidAdminPassword):
		writeError(w, http.StatusBadRequest, errors.New("密码至少需要 8 个字符、不能超过 72 字节，且不能包含控制字符"))
	case errors.Is(err, store.ErrAdminUsernameTaken):
		writeError(w, http.StatusConflict, errors.New("用户名已存在"))
	case errors.Is(err, store.ErrAdminUserLastEnabled):
		writeError(w, http.StatusConflict, errors.New("必须保留至少一个启用的管理员"))
	case errors.Is(err, store.ErrAdminUserNotFound):
		writeError(w, http.StatusNotFound, errors.New("管理员不存在"))
	case errors.Is(err, store.ErrAdminOwnerProtected):
		writeError(w, http.StatusConflict, errors.New("最高权限管理员不能停用或删除"))
	case errors.Is(err, store.ErrInvalidAdminInvite):
		writeError(w, http.StatusConflict, errors.New("邀请码无效、已使用或已吊销"))
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

// adminUserID 接受服务端生成的十六进制 ID 和一次性旧版迁移 ID。
// 先约束长度/字符，再进入参数化 SQL，避免异常 PathValue 进入控制面状态。
func adminUserID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return "", false
	}
	for _, current := range value {
		if (current < 'a' || current > 'z') && (current < '0' || current > '9') && current != '-' {
			return "", false
		}
	}
	return value, true
}
