package serverapp

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"time"

	"smalux-speedtest/internal/importer"
	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
)

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
