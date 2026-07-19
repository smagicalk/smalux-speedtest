package serverapp

import (
	"net/http"
	"strings"

	"smalux-speedtest/internal/store"
)

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
