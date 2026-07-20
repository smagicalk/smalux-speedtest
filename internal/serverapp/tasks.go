package serverapp

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"time"
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

// createTask 解码管理页面请求，再把与入口无关的创建流程交给 startTask。
// Telegram Bot 也调用同一服务，从而共享订阅限制、协议解析、参数校验和调度语义。
func (a *App) createTask(w http.ResponseWriter, r *http.Request) {
	var input taskRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, err := a.startTask(r.Context(), input)
	if err != nil {
		var requestError *taskRequestError
		if errors.As(err, &requestError) {
			if len(requestError.ImportErrors) > 0 {
				writeJSON(w, requestError.Status, map[string]any{"error": requestError.Error(), "import_errors": requestError.ImportErrors})
			} else {
				writeError(w, requestError.Status, requestError)
			}
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"task": created.Task, "import_errors": created.ImportErrors})
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
	w.Header().Set("Cache-Control", "no-store")
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
		case <-a.hub.Done():
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
			csvCell(result.ClientID), csvCell(result.ProxyName), result.Protocol, result.MaskedAddress, csvCell(result.SpeedServerID),
			csvCell(result.SpeedServerName), formatFloat(result.LatencyMS), formatFloat(result.JitterMS),
			formatFloat(result.DownloadBPS / 1_000_000), formatFloat(result.UploadBPS / 1_000_000), csvCell(result.Error), result.CreatedAt,
		})
	}
	writer.Flush()
}
