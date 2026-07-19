package serverapp

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/wire"
)

// TestApplyTaskDefaults 验证只有零值会替换成默认值，显式设置的合法值和待校验的负数
// 均保持原样。这样调用方能区分“未指定”与“指定了非法参数”。
func TestApplyTaskDefaults(t *testing.T) {
	tests := []struct {
		name  string
		input taskRequest
		want  taskRequest
	}{
		{
			name:  "all unspecified",
			input: taskRequest{},
			want:  taskRequest{CandidateCount: 10, TopN: 3, Threads: 4},
		},
		{
			name:  "partial defaults",
			input: taskRequest{CandidateCount: 20, Threads: 8},
			want:  taskRequest{CandidateCount: 20, TopN: 3, Threads: 8},
		},
		{
			name:  "explicit boundary values",
			input: taskRequest{CandidateCount: 50, TopN: 1, Threads: 32},
			want:  taskRequest{CandidateCount: 50, TopN: 1, Threads: 32},
		},
		{
			name:  "negative values remain invalid",
			input: taskRequest{CandidateCount: -1, TopN: -2, Threads: -3},
			want:  taskRequest{CandidateCount: -1, TopN: -2, Threads: -3},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			applyTaskDefaults(&test.input)
			if test.input.CandidateCount != test.want.CandidateCount || test.input.TopN != test.want.TopN || test.input.Threads != test.want.Threads {
				t.Fatalf("defaults = candidate:%d top:%d threads:%d, want candidate:%d top:%d threads:%d",
					test.input.CandidateCount, test.input.TopN, test.input.Threads,
					test.want.CandidateCount, test.want.TopN, test.want.Threads)
			}
		})
	}
}

// TestValidateTaskParameters 使用最小值、最大值和参数间约束覆盖全部接受/拒绝边界。
func TestValidateTaskParameters(t *testing.T) {
	tests := []struct {
		name    string
		input   taskRequest
		wantErr bool
	}{
		{name: "minimum", input: taskRequest{CandidateCount: 1, TopN: 1, Threads: 1}},
		{name: "maximum", input: taskRequest{CandidateCount: 50, TopN: 3, Threads: 32}},
		{name: "top equals candidates", input: taskRequest{CandidateCount: 2, TopN: 2, Threads: 4}},
		{name: "candidate below minimum", input: taskRequest{CandidateCount: -1, TopN: 1, Threads: 4}, wantErr: true},
		{name: "candidate above maximum", input: taskRequest{CandidateCount: 51, TopN: 1, Threads: 4}, wantErr: true},
		{name: "top below minimum", input: taskRequest{CandidateCount: 10, TopN: -1, Threads: 4}, wantErr: true},
		{name: "top above maximum", input: taskRequest{CandidateCount: 10, TopN: 4, Threads: 4}, wantErr: true},
		{name: "top exceeds candidates", input: taskRequest{CandidateCount: 1, TopN: 2, Threads: 4}, wantErr: true},
		{name: "threads below minimum", input: taskRequest{CandidateCount: 10, TopN: 3, Threads: -1}, wantErr: true},
		{name: "threads above maximum", input: taskRequest{CandidateCount: 10, TopN: 3, Threads: 33}, wantErr: true},
		{name: "raw zero values require defaults first", input: taskRequest{}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateTaskParameters(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateTaskParameters(%+v) error = %v, wantErr %v", test.input, err, test.wantErr)
			}
		})
	}
}

// TestStartTaskRejectsNoClients 验证 Client 选择是创建任务的前置条件。即使请求包含订阅
// URL，也必须在触发网络抓取前返回可安全展示的 400 错误。
func TestStartTaskRejectsNoClients(t *testing.T) {
	application := &App{}
	_, err := application.startTask(t.Context(), taskRequest{
		Source:          "socks5://example.com:1080#node",
		SubscriptionURL: "http://127.0.0.1/should-not-be-fetched",
	})
	var requestError *taskRequestError
	if !errors.As(err, &requestError) {
		t.Fatalf("error = %v, want taskRequestError", err)
	}
	if requestError.Status != http.StatusBadRequest || requestError.Message != "select at least one client" {
		t.Fatalf("unexpected request error: %+v", requestError)
	}
}

// TestValidateTaskClients 验证数据库 Client 快照上的校验语义：重复 ID 按首次出现顺序
// 去重，而未知或已撤销的 Client 均作为调用方错误拒绝。
func TestValidateTaskClients(t *testing.T) {
	application := newTaskServiceTestApp(t)
	first, _, err := application.store.CreateClient(t.Context(), "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := application.store.CreateClient(t.Context(), "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	revoked, _, err := application.store.CreateClient(t.Context(), "revoked", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.store.RevokeClient(t.Context(), revoked.ID); err != nil {
		t.Fatal(err)
	}

	clientIDs, err := application.validateTaskClients(t.Context(), []string{second.ID, first.ID, second.ID, first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(clientIDs) != 2 || clientIDs[0] != second.ID || clientIDs[1] != first.ID {
		t.Fatalf("deduplicated clients = %v, want [%s %s]", clientIDs, second.ID, first.ID)
	}
	for _, id := range []string{"unknown-client", revoked.ID} {
		_, err := application.validateTaskClients(t.Context(), []string{id})
		var requestError *taskRequestError
		if !errors.As(err, &requestError) || requestError.Status != http.StatusBadRequest {
			t.Fatalf("client %q error = %v, want bad request", id, err)
		}
	}
}

// TestStartTaskAppliesDefaultsAndDeduplicatesClients 以临时 SQLite 和真实 Hub 验证纯逻辑
// 组合后的创建结果：默认参数进入持久化摘要与 Assignment，重复 Client 不会增加目标数。
func TestStartTaskAppliesDefaultsAndDeduplicatesClients(t *testing.T) {
	application := newTaskServiceTestApp(t)
	first, _, err := application.store.CreateClient(t.Context(), "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := application.store.CreateClient(t.Context(), "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	created, err := application.startTask(t.Context(), taskRequest{
		Source:    "socks5://example.com:1080#node",
		ClientIDs: []string{second.ID, first.ID, second.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	// CancelTask 停止 10 分钟到期定时器，避免测试结束后保留后台回调。
	defer func() {
		if err := application.hub.CancelTask(t.Context(), created.Task.ID); err != nil {
			t.Errorf("cancel task: %v", err)
		}
	}()
	if created.Task.CandidateCount != 10 || created.Task.TopN != 3 || created.Task.Threads != 4 || created.Task.ClientCount != 2 || created.Task.ProxyCount != 1 {
		t.Fatalf("unexpected created task: %+v", created.Task)
	}
	stored, err := application.store.GetTask(t.Context(), created.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CandidateCount != 10 || stored.TopN != 3 || stored.Threads != 4 || stored.ClientCount != 2 {
		t.Fatalf("unexpected stored task: %+v", stored)
	}
	application.hub.mu.RLock()
	runtime := application.hub.tasks[created.Task.ID]
	if runtime == nil {
		application.hub.mu.RUnlock()
		t.Fatal("task was not added to Hub")
	}
	assignment := runtime.assignment
	targetCount := len(runtime.targets)
	_, firstTarget := runtime.targets[first.ID]
	_, secondTarget := runtime.targets[second.ID]
	application.hub.mu.RUnlock()
	if assignment.CandidateCount != 10 || assignment.TopN != 3 || assignment.Threads != 4 || len(assignment.Proxies) != 1 {
		t.Fatalf("unexpected assignment: %+v", assignment)
	}
	if targetCount != 2 || !firstTarget || !secondTarget {
		t.Fatalf("unexpected Hub targets: count=%d first=%v second=%v", targetCount, firstTarget, secondTarget)
	}
}

// TestValidateTaskAssignmentSize 使用真实 Envelope 编码验证批量任务必须适配 Client
// 读上限。拒绝发生在 Store.CreateTask 前，因此调用方不会留下无法下发的 queued 行。
func TestValidateTaskAssignmentSize(t *testing.T) {
	normal := testAssignment("normal-task")
	if err := validateTaskAssignmentSize(normal); err != nil {
		t.Fatalf("normal assignment was rejected: %v", err)
	}
	oversized := model.Assignment{
		TaskID: "oversized-task", CandidateCount: 1, TopN: 1, Threads: 1,
		Proxies: []model.ProxySpec{{
			ID: "proxy", Name: strings.Repeat("x", wire.MaxServerToClientMessageBytes),
			Protocol: "socks", Server: "example.com", Port: 1080,
			Outbound: []byte(`{"type":"socks","tag":"proxy","server":"example.com","server_port":1080}`),
		}},
	}
	err := validateTaskAssignmentSize(oversized)
	var requestError *taskRequestError
	if !errors.As(err, &requestError) || requestError.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized assignment error = %v, want 413 taskRequestError", err)
	}
}

// newTaskServiceTestApp 创建只包含任务服务所需依赖的 App，避免启动 HTTP 监听或使用
// 网络订阅。每个测试使用独立 SQLite 文件并自动关闭。
func newTaskServiceTestApp(t *testing.T) *App {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "task-service.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &App{store: database, hub: NewHub(database, logger)}
}
