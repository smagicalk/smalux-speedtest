package serverapp

import (
	"errors"
	"net/http"

	"smalux-speedtest/internal/store"
)

// accountPage renders personal security controls for every authenticated
// administrator. System-wide Bot and account administration remain Owner-only.
func (a *App) accountPage(w http.ResponseWriter, r *http.Request) {
	session, _ := a.session(r)
	_ = a.template.ExecuteTemplate(w, "account.html", pageData{
		CSRF: session.csrf, CurrentAdminID: session.userID, Username: session.username, IsOwner: session.isOwner,
	})
}

// changeOwnPassword verifies the current password, persists the replacement hash and
// rotates every login session belonging to this account. The response installs one
// fresh session for the request that performed the change; all other devices must log
// in again.
func (a *App) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var request struct {
		CurrentPassword    string `json:"current_password"`
		NewPassword        string `json:"new_password"`
		NewPasswordConfirm string `json:"new_password_confirm"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("密码信息格式无效"))
		return
	}
	if request.NewPassword != request.NewPasswordConfirm {
		writeError(w, http.StatusBadRequest, errors.New("两次输入的新密码不一致"))
		return
	}

	currentSession, _ := a.session(r)
	err := a.store.ChangeAdminPassword(r.Context(), currentSession.userID, request.CurrentPassword, request.NewPassword)
	switch {
	case errors.Is(err, store.ErrInvalidAdminCredentials):
		// Session 本身仍然有效，不能返回 401，否则前端会把它解释为登录过期。
		writeError(w, http.StatusBadRequest, errors.New("当前密码错误"))
		return
	case errors.Is(err, store.ErrInvalidAdminPassword):
		writeError(w, http.StatusBadRequest, errors.New("新密码需为 8-72 字节且不能包含控制字符"))
		return
	case errors.Is(err, store.ErrAdminPasswordUnchanged):
		writeError(w, http.StatusBadRequest, errors.New("新密码不能与当前密码相同"))
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	a.sessions.deleteUser(currentSession.userID)
	newSession := a.sessions.create(currentSession.userID, currentSession.username, currentSession.isOwner)
	setSessionCookie(w, r, newSession)
	writeJSON(w, http.StatusOK, map[string]bool{"changed": true})
}
