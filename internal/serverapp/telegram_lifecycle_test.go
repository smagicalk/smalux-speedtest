package serverapp

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewRejectsIncompleteTelegramConfiguration 验证 Token 与 owner ID 必须成对配置。
// 两种失败都应发生在 Bot 启动和网络请求之前，同时 New 不返回半初始化 App。
func TestNewRejectsIncompleteTelegramConfiguration(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		ownerID   int64
		wantError string
	}{
		{
			name:      "token without owner",
			token:     "123456:test-token",
			wantError: "telegram owner ID must be positive",
		},
		{
			name:      "owner without token",
			ownerID:   1001,
			wantError: "telegram bot token is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			application, err := New(t.Context(), Config{
				Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "incomplete.db"), AdminPassword: "test-password",
				TelegramBotToken: test.token, TelegramOwnerID: test.ownerID,
				// 即使实现顺序意外变化，也只可能访问本机不可用端口，绝不会回落到公网 API。
				TelegramAPIBaseURL: "http://127.0.0.1:1",
				Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			if err == nil {
				if application != nil {
					shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
					_ = application.Shutdown(shutdownCtx)
					cancel()
				}
				t.Fatal("New accepted incomplete Telegram configuration")
			}
			if application != nil {
				t.Fatalf("New returned a partial App on error: %+v", application)
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("New error = %q, want substring %q", err, test.wantError)
			}
		})
	}
}

// TestTelegramLifecycleSynchronizesOwnerAndStopsPolling 使用返回空更新的本地 getUpdates
// 验证完整生命周期：New 同步唯一 owner 并启动轮询，Shutdown 停止后续请求、等待 Bot
// 退出，最后正常关闭尚未启动监听的 HTTP Server 和 SQLite。
func TestTelegramLifecycleSynchronizesOwnerAndStopsPolling(t *testing.T) {
	const (
		token   = "123456:test-token"
		ownerID = int64(987654321)
	)
	pollStarted := make(chan struct{})
	var startOnce sync.Once
	var pollCount atomic.Int64
	fakeAPI := newTelegramLifecycleServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/bot"+token+"/getUpdates" {
			http.NotFound(w, r)
			return
		}
		pollCount.Add(1)
		startOnce.Do(func() { close(pollStarted) })
		// 空更新是 Telegram getUpdates 的合法响应。Bot 会继续轮询，请求计数因而可以
		// 用来确认 Shutdown 返回后后台循环已经停止且不再访问 API。
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
	}))
	defer fakeAPI.Close()

	application, err := New(t.Context(), Config{
		Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "telegram-lifecycle.db"), AdminPassword: "test-password",
		TelegramBotToken: token, TelegramOwnerID: ownerID, TelegramAPIBaseURL: fakeAPI.URL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	shutdownComplete := false
	t.Cleanup(func() {
		if shutdownComplete {
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := application.Shutdown(shutdownCtx); err != nil {
			t.Errorf("cleanup Shutdown: %v", err)
		}
	})

	select {
	case <-pollStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("Telegram Bot did not start getUpdates polling")
	}
	if application.telegram == nil {
		t.Fatal("New did not retain Telegram runtime")
	}
	authorized, err := application.store.IsTelegramAuthorized(t.Context(), ownerID)
	if err != nil || !authorized {
		t.Fatalf("owner authorization = %v, %v", authorized, err)
	}
	owner, err := application.store.IsTelegramOwner(t.Context(), ownerID)
	if err != nil || !owner {
		t.Fatalf("owner recognition = %v, %v", owner, err)
	}
	users, err := application.store.ListTelegramUsers(t.Context())
	if err != nil || len(users) != 1 || users[0].TelegramID != ownerID || !users[0].Owner {
		t.Fatalf("unexpected synchronized Telegram users: %+v, %v", users, err)
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	err = application.Shutdown(shutdownCtx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	shutdownComplete = true
	select {
	case <-application.telegram.done:
	default:
		t.Fatal("Shutdown returned before Telegram Bot goroutine exited")
	}
	requestsAtShutdown := pollCount.Load()
	time.Sleep(50 * time.Millisecond)
	if current := pollCount.Load(); current != requestsAtShutdown {
		t.Fatalf("Telegram polling continued after Shutdown: requests %d -> %d", requestsAtShutdown, current)
	}
}

// newTelegramLifecycleServer 创建只绑定 IPv4 loopback 的 httptest Server。受限沙箱若
// 禁止本地监听则跳过依赖 socket 的生命周期用例；任何情况下都不会访问公网。
func newTelegramLifecycleServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}
