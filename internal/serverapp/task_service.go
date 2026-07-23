package serverapp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"smalux-speedtest/internal/importer"
	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/wire"
)

// taskRequest 是管理页面和其他可信控制面入口共用的任务创建参数。
// Source 与 SubscriptionURL 可以同时提供；ClientIDs 必须由调用方显式选择。
type taskRequest struct {
	Source          string   `json:"source"`
	SubscriptionURL string   `json:"subscription_url"`
	ClientIDs       []string `json:"client_ids"`
	CandidateCount  int      `json:"candidate_count"`
	TopN            int      `json:"top_n"`
	Threads         int      `json:"threads"`
}

// taskCreation 汇总成功创建的持久化任务和非致命的逐条导入错误。
// 只要至少一个代理有效，ImportErrors 非空也不会阻止任务下发。
type taskCreation struct {
	Task         store.Task
	ImportErrors []model.ImportError
}

// taskRequestError 表示由调用方输入导致、可以直接安全展示的失败。
// Status 供 HTTP 入口映射响应码；Bot 等非 HTTP 入口只需使用 Error 文本。
type taskRequestError struct {
	Status       int
	Message      string
	ImportErrors []model.ImportError
}

func (e *taskRequestError) Error() string { return e.Message }

// startTask 导入代理、校验参数与目标 Client，然后持久化并下发测速任务。
//
// store.CreateTask 只保存任务摘要；parsed.Proxies 中的密码、UUID、私钥等敏感字段仅进入
// Hub 的内存 Assignment，并在任务终结时释放，绝不会写入 SQLite。
func (a *App) startTask(ctx context.Context, input taskRequest) (taskCreation, error) {
	if len(input.ClientIDs) == 0 {
		return taskCreation{}, invalidTaskRequest(http.StatusBadRequest, "select at least one client")
	}
	// Fetcher 对目标地址、重定向和响应大小实施 SSRF 防护；获取后的正文与直接输入统一
	// 进入 importer，因此普通文本和 Base64 订阅具有相同解析行为。
	if input.SubscriptionURL != "" {
		content, err := a.fetcher.Fetch(ctx, input.SubscriptionURL)
		if err != nil {
			// 网络库错误可能回显包含鉴权查询参数的完整 URL。控制面只返回固定提示，
			// Fetcher 的具体失败既不落库也不写日志。
			return taskCreation{}, &taskRequestError{Status: http.StatusBadRequest, Message: "订阅获取失败，请检查地址后重试"}
		}
		if input.Source != "" {
			input.Source += "\n"
		}
		input.Source += content
	}
	parsed := importer.Parse(input.Source)
	if len(parsed.Proxies) == 0 {
		return taskCreation{}, &taskRequestError{
			Status: http.StatusUnprocessableEntity, Message: "没有可测速的代理", ImportErrors: parsed.Errors,
		}
	}
	applyTaskDefaults(&input)
	if err := validateTaskParameters(input); err != nil {
		return taskCreation{}, &taskRequestError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	clientIDs, err := a.validateTaskClients(ctx, input.ClientIDs)
	if err != nil {
		var requestError *taskRequestError
		if errors.As(err, &requestError) {
			return taskCreation{}, requestError
		}
		return taskCreation{}, err
	}

	task := store.Task{
		ID: model.NewID(), Status: "queued", CandidateCount: input.CandidateCount, TopN: input.TopN, Threads: input.Threads,
		ProxyCount: len(parsed.Proxies), ClientCount: len(clientIDs), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	assignment := model.Assignment{
		TaskID: task.ID, Proxies: parsed.Proxies, CandidateCount: input.CandidateCount,
		TopN: input.TopN, Threads: input.Threads, TimeoutSeconds: 600,
	}
	// 必须在持久化任务前按最终 wire Envelope 检查大小。否则超大订阅会先留下 queued
	// 任务，再让 Client 因超过 WebSocket ReadLimit 反复断线直到整体超时。
	if err := validateTaskAssignmentSize(assignment); err != nil {
		return taskCreation{}, err
	}
	if err := a.store.CreateTask(ctx, task, clientIDs); err != nil {
		return taskCreation{}, err
	}
	a.hub.AddTask(assignment, clientIDs)
	return taskCreation{Task: task, ImportErrors: parsed.Errors}, nil
}

// validateTaskAssignmentSize checks the largest single-proxy work unit actually sent
// by protocol v2. The aggregate subscription may exceed one WebSocket frame because
// it is retained only in Server memory and dispatched one proxy at a time.
func validateTaskAssignmentSize(assignment model.Assignment) error {
	for index, proxy := range assignment.Proxies {
		unit := assignment
		unit.WorkID = "00000000000000000000000000000000"
		unit.ProxyIndex = index + 1
		unit.ProxyTotal = len(assignment.Proxies)
		unit.Proxies = []model.ProxySpec{proxy}
		message, err := wire.New(wire.TypeTaskAssign, assignment.TaskID, unit)
		if err != nil {
			return err
		}
		size, err := wire.EncodedSize(message)
		if err != nil {
			return err
		}
		if size > wire.MaxServerToClientMessageBytes {
			return &taskRequestError{Status: http.StatusRequestEntityTooLarge, Message: "单个代理配置过大，无法下发"}
		}
	}
	return nil
}

// applyTaskDefaults 只替换未指定的零值；负数和越界正数留给校验明确拒绝。
func applyTaskDefaults(input *taskRequest) {
	if input.CandidateCount == 0 {
		input.CandidateCount = 10
	}
	if input.TopN == 0 {
		input.TopN = 3
	}
	if input.Threads == 0 {
		input.Threads = 4
	}
}

func validateTaskParameters(input taskRequest) error {
	if input.CandidateCount < 1 || input.CandidateCount > 50 || input.TopN < 1 || input.TopN > 3 || input.TopN > input.CandidateCount || input.Threads < 1 || input.Threads > 32 {
		return errors.New("candidate_count must be 1-50, top_n must be 1-3, and threads must be 1-32")
	}
	return nil
}

// validateTaskClients 对数据库快照校验 Client，并在保持提交顺序的同时去重。
// 是否在线不属于通用校验条件：网页允许创建等待离线 Client 重连的 queued 任务。
func (a *App) validateTaskClients(ctx context.Context, requested []string) ([]string, error) {
	clients, err := a.store.ListClients(ctx)
	if err != nil {
		return nil, err
	}
	valid := make(map[string]bool, len(clients))
	for _, client := range clients {
		valid[client.ID] = client.Enabled
	}
	unique := make([]string, 0, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, id := range requested {
		if !valid[id] {
			return nil, invalidTaskRequest(http.StatusBadRequest, "selected client does not exist or is revoked")
		}
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	return unique, nil
}

func invalidTaskRequest(status int, message string) *taskRequestError {
	return &taskRequestError{Status: status, Message: message}
}
