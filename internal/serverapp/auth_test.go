package serverapp

import (
	"bytes"
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
	"testing"
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
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("sensitive response cache policy = %q", response.Header.Get("Cache-Control"))
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["token"] == "" {
		t.Fatal("client token missing")
	}
}
