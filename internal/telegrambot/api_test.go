package telegrambot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHTTPAPI 覆盖长轮询 JSON、纯文本 JSON 和图片 multipart 三种 Telegram 调用形式。
func TestHTTPAPI(t *testing.T) {
	const token = "123456:test-token"
	var gotOffset, gotTimeout int64
	var gotAllowed []string
	var gotMessage string
	var gotChatID int64
	var gotPhoto, gotFilename, gotCaption string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+token+"/") {
			t.Errorf("unexpected Telegram path: %s", r.URL.Path)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			var input struct {
				Offset         int64    `json:"offset"`
				Timeout        int64    `json:"timeout"`
				AllowedUpdates []string `json:"allowed_updates"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode getUpdates request: %v", err)
			}
			gotOffset, gotTimeout, gotAllowed = input.Offset, input.Timeout, input.AllowedUpdates
			return telegramResponse(http.StatusOK, `{"ok":true,"result":[{"update_id":42,"message":{"message_id":7,"from":{"id":99},"chat":{"id":99,"type":"private"},"text":"/id"}}]}`), nil
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var input struct {
				ChatID int64  `json:"chat_id"`
				Text   string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode sendMessage request: %v", err)
			}
			gotChatID, gotMessage = input.ChatID, input.Text
			return telegramResponse(http.StatusOK, `{"ok":true,"result":{}}`), nil
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse sendPhoto form: %v", err)
			}
			gotCaption = r.FormValue("caption")
			file, header, err := r.FormFile("photo")
			if err != nil {
				t.Errorf("read sendPhoto file: %v", err)
			} else {
				data, _ := io.ReadAll(file)
				file.Close()
				gotPhoto, gotFilename = string(data), header.Filename
			}
			return telegramResponse(http.StatusOK, `{"ok":true,"result":{}}`), nil
		default:
			return telegramResponse(http.StatusNotFound, `{"ok":false,"description":"not found"}`), nil
		}
	})}
	api, err := NewHTTPAPI(HTTPConfig{Token: token, BaseURL: "https://telegram.test", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	updates, err := api.GetUpdates(t.Context(), 40, 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].UpdateID != 42 || updates[0].Message == nil || updates[0].Message.Text != "/id" {
		t.Fatalf("unexpected updates: %+v", updates)
	}
	if gotOffset != 40 || gotTimeout != 2 || len(gotAllowed) != 1 || gotAllowed[0] != "message" {
		t.Fatalf("unexpected poll input: offset=%d timeout=%d allowed=%v", gotOffset, gotTimeout, gotAllowed)
	}
	if err := api.SendMessage(t.Context(), 88, "hello"); err != nil {
		t.Fatal(err)
	}
	if gotChatID != 88 || gotMessage != "hello" {
		t.Fatalf("unexpected message: chat=%d text=%q", gotChatID, gotMessage)
	}
	if err := api.SendPhoto(t.Context(), 88, "result.png", "caption", []byte("PNG")); err != nil {
		t.Fatal(err)
	}
	if gotFilename != "result.png" || gotCaption != "caption" || gotPhoto != "PNG" {
		t.Fatalf("unexpected photo: filename=%q caption=%q data=%q", gotFilename, gotCaption, gotPhoto)
	}
}

// TestHTTPAPIError 验证 Telegram 的业务错误被转换成包含方法和错误码的 APIError。
func TestHTTPAPIError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramResponse(http.StatusUnauthorized, `{"ok":false,"error_code":401,"description":"Unauthorized"}`), nil
	})}
	api, err := NewHTTPAPI(HTTPConfig{Token: "bad", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	err = api.SendMessage(t.Context(), 1, "test")
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.ErrorCode != 401 || apiError.Method != "sendMessage" {
		t.Fatalf("unexpected error: %#v", err)
	}
}

// TestHTTPAPIBusinessErrorWithSuccessfulHTTPStatus 保留 Telegram 在 HTTP 200
// 中返回 ok=false 的 error_code，供限流重试逻辑正确识别 429。
func TestHTTPAPIBusinessErrorWithSuccessfulHTTPStatus(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramResponse(http.StatusOK, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":3}}`), nil
	})}
	api, err := NewHTTPAPI(HTTPConfig{Token: "token", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	err = api.SendMessage(t.Context(), 1, "test")
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusOK || apiError.ErrorCode != 429 || apiError.RetryAfterSeconds != 3 {
		t.Fatalf("unexpected business error: %#v", err)
	}
}

// TestHTTPAPITransportErrorRedactsToken 防止 net/http 的 *url.Error 泄露 URL 内嵌的 Bot Token。
func TestHTTPAPITransportErrorRedactsToken(t *testing.T) {
	const token = "999999:top-secret-token"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed for " + request.URL.String())
	})}
	api, err := NewHTTPAPI(HTTPConfig{Token: token, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	err = api.SendMessage(t.Context(), 1, "test")
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("transport error leaked token: %v", err)
	}
}

// TestHTTPAPIResponseErrorRedactsToken 覆盖自建 API 或异常网关在 description
// 中回显请求 URL 的情况，确保 APIError 后续写日志也不含 Token。
func TestHTTPAPIResponseErrorRedactsToken(t *testing.T) {
	const token = "999999:response-secret"
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"ok":false,"error_code":500,"description":"upstream /bot` + token + `/sendMessage failed"}`
		return telegramResponse(http.StatusInternalServerError, body), nil
	})}
	api, err := NewHTTPAPI(HTTPConfig{Token: token, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	err = api.SendMessage(t.Context(), 1, "test")
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("response error was not redacted: %v", err)
	}
}

// TestHTTPAPIPreservesCancellation 确保取消语义不被脱敏包装吞掉。
func TestHTTPAPIPreservesCancellation(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})}
	api, err := NewHTTPAPI(HTTPConfig{Token: "token", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	err = api.SendMessage(t.Context(), 1, "test")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

// TestHTTPAPIDisablesRedirectsByDefault 防止 URL 路径中的 Bot Token 被默认重定向
// 到其他主机；注入 Client 时使用副本，不修改调用方持有的对象。
func TestHTTPAPIDisablesRedirectsByDefault(t *testing.T) {
	client := &http.Client{}
	api, err := NewHTTPAPI(HTTPConfig{Token: "token", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if api.client == client || client.CheckRedirect != nil || api.client.CheckRedirect == nil {
		t.Fatal("injected HTTP client was not safely cloned")
	}
	if err := api.client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy returned %v", err)
	}
}

// TestHTTPAPIRejectsRemotePlaintextBaseURL 防止自建 API 配置把 URL 路径中的 Bot Token
// 明文发送到远程网络；loopback HTTP 仍供本地 Bot API Server 和测试使用。
func TestHTTPAPIRejectsRemotePlaintextBaseURL(t *testing.T) {
	if _, err := NewHTTPAPI(HTTPConfig{Token: "token", BaseURL: "http://example.com"}); err == nil {
		t.Fatal("remote plaintext Telegram API URL was accepted")
	}
	for _, baseURL := range []string{"http://127.0.0.1:8081", "http://[::1]:8081", "http://bot.localhost:8081", "https://example.com"} {
		if _, err := NewHTTPAPI(HTTPConfig{Token: "token", BaseURL: baseURL}); err != nil {
			t.Fatalf("safe base URL %q rejected: %v", baseURL, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func telegramResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
