package serverapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/wire"
)

// TestAdminLoginAndCreateClient 覆盖管理端最小安全闭环：
//  1. 用首次引导的管理员密码登录并取得 HttpOnly Session Cookie；
//  2. 从受保护控制台 HTML 读取当前会话的 CSRF Token；
//  3. 同时携带 Cookie 与 CSRF Header 调用写接口创建 Client；
//  4. 确认明文 Client Token 只通过创建响应交付。
func TestAdminLoginAndCreateClient(t *testing.T) {
	// 每个测试使用独立临时 SQLite 文件，避免共享管理员、Client 或任务状态。
	application, err := New(t.Context(), Config{
		Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "app.db"), AdminPassword: "test-password",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	// 显式绑定 IPv4 回环随机端口，既不暴露测试服务，也兼容禁止本地 socket 的沙箱。
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(application.routes())
	server.Listener = listener
	server.Start()
	defer server.Close()
	// CookieJar 模拟浏览器自动保存登录响应 Cookie，并在后续受保护请求中携带它。
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	response, err := client.PostForm(server.URL+"/login", url.Values{"password": {"test-password"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	// CSRF Token 由服务端模板注入 meta 标签，真实前端脚本也从同一位置读取。
	match := regexp.MustCompile(`name="csrf-token" content="([^"]+)"`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("CSRF token not found in dashboard: %s", body)
	}

	// 写 API 需要 Session Cookie（由 Jar 添加）和匹配的 X-CSRF-Token（显式添加）。
	payload, _ := json.Marshal(map[string]any{"name": "test-client", "labels": map[string]string{"region": "test"}})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/clients", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", string(match[1]))
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("create client returned %d: %s", response.StatusCode, body)
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["token"] == "" {
		t.Fatal("client token missing")
	}
}

// TestWebSocketTaskLifecycle 以真实本地 WebSocket 验证一条任务从连接到持久化终态的流程：
// Client Token 认证 -> hello/welcome 握手 -> Assignment 下发 -> ACK 进入 running ->
// result 落库 -> complete 聚合任务为 completed。测试还确认结果 ClientID 取自认证连接，
// 而不是由不可信结果载荷决定。
func TestWebSocketTaskLifecycle(t *testing.T) {
	application, err := New(t.Context(), Config{
		Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "ws.db"), AdminPassword: "test-password",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(application.routes())
	server.Listener = listener
	server.Start()
	defer server.Close()

	// 直接通过 store 创建凭据，以便本测试聚焦 Client 协议而非重复管理端登录流程。
	clientRecord, token, err := application.store.CreateClient(t.Context(), "ws-client", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Bearer Token 在 HTTP Upgrade 阶段认证；管理员 Session 不适用于 Client WebSocket。
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+token)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/client"
	wsCtx, wsCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer wsCancel()
	connection, _, err := websocket.Dial(wsCtx, wsURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	// WebSocket 建立后，第一帧必须是当前协议版本的 hello，服务端随后返回 welcome。
	hello, _ := wire.New(wire.TypeHello, "", model.Hello{Name: "ws-client", Version: "test", OS: "linux", Arch: "amd64"})
	if err := wsjson.Write(wsCtx, connection, hello); err != nil {
		t.Fatal(err)
	}
	var welcome wire.Envelope
	if err := wsjson.Read(wsCtx, connection, &welcome); err != nil || welcome.Type != wire.TypeWelcome {
		t.Fatalf("invalid welcome: %+v %v", welcome, err)
	}

	// 与生产 createTask 保持相同顺序：先持久化不含代理秘密的任务摘要和目标，再将完整
	// Assignment 加入 Hub。测试 outbound 仅为固定假数据，不会真正发起测速连接。
	taskID := model.NewID()
	task := store.Task{ID: taskID, Status: "queued", CandidateCount: 1, TopN: 1, ProxyCount: 1, ClientCount: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := application.store.CreateTask(t.Context(), task, []string{clientRecord.ID}); err != nil {
		t.Fatal(err)
	}
	assignment := model.Assignment{TaskID: taskID, CandidateCount: 1, TopN: 1, TimeoutSeconds: 60, Proxies: []model.ProxySpec{{ID: "proxy", Name: "node", Protocol: "socks", Server: "example.com", Port: 1080, Outbound: json.RawMessage(`{"type":"socks","tag":"proxy","server":"example.com","server_port":1080}`)}}}
	application.hub.AddTask(assignment, []string{clientRecord.ID})
	// Client 在线，因此 AddTask 应立即发送 task.assign；服务端等待 ACK 后才接受结果。
	var assigned wire.Envelope
	if err := wsjson.Read(wsCtx, connection, &assigned); err != nil || assigned.Type != wire.TypeTaskAssign {
		t.Fatalf("invalid assignment: %+v %v", assigned, err)
	}
	ack, _ := wire.New(wire.TypeTaskAck, taskID, model.Ack{TaskID: taskID})
	if err := wsjson.Write(wsCtx, connection, ack); err != nil {
		t.Fatal(err)
	}
	// 结果载荷故意不设置 ClientID，验证 Hub 会使用已认证 peer 的 Client ID 覆盖身份。
	resultMessage, _ := wire.New(wire.TypeTaskResult, taskID, model.SpeedResult{
		TaskID: taskID, ProxyID: "proxy", ProxyName: "node", Protocol: "socks", MaskedAddress: "*.example.com:1080", SpeedServerID: "1", LatencyMS: 12,
	})
	if err := wsjson.Write(wsCtx, connection, resultMessage); err != nil {
		t.Fatal(err)
	}
	complete, _ := wire.New(wire.TypeTaskComplete, taskID, model.Ack{TaskID: taskID})
	if err := wsjson.Write(wsCtx, connection, complete); err != nil {
		t.Fatal(err)
	}

	// WebSocket 读循环和 SQLite 更新异步执行，使用有总超时的短轮询等待最终状态，避免
	// 固定 sleep 让测试不必要地变慢或在较慢机器上不稳定。
	deadline, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		stored, getErr := application.store.GetTask(deadline, taskID)
		if getErr == nil && stored.Status == "completed" {
			results, listErr := application.store.ListResults(deadline, taskID)
			if listErr != nil || len(results) != 1 || results[0].ClientID != clientRecord.ID {
				t.Fatalf("unexpected results: %+v %v", results, listErr)
			}
			break
		}
		select {
		case <-deadline.Done():
			t.Fatalf("task did not complete: %+v %v", stored, getErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
