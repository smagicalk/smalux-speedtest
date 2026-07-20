package telegrambot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
)

// execute 验证 HTTP 状态和 Telegram ok 字段，并按需解码 result。
func (a *HTTPAPI) execute(request *http.Request, method string, output any) error {
	response, err := a.client.Do(request)
	if err != nil {
		// net/http 通常返回 *url.Error，其 Error 文本包含完整请求 URL，而 Telegram URL
		// 内嵌 Bot Token。取消语义可直接保留；其他错误只返回移除 Token 后的诊断文本。
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return fmt.Errorf("telegram %s request failed: %s", method, a.redact(err.Error()))
	}
	defer response.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("decode telegram %s response: %w", method, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.OK {
		return &APIError{
			Method:            method,
			StatusCode:        response.StatusCode,
			ErrorCode:         envelope.ErrorCode,
			RetryAfterSeconds: maxPositive(envelope.Parameters.RetryAfter),
			Description:       a.redact(envelope.Description),
		}
	}
	if output != nil && len(envelope.Result) != 0 && string(envelope.Result) != "null" {
		if err := json.Unmarshal(envelope.Result, output); err != nil {
			return fmt.Errorf("decode telegram %s result: %w", method, err)
		}
	}
	return nil
}

// maxPositive discards malformed negative retry hints before they reach retry arithmetic.
func maxPositive(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

// methodURL 构造 Telegram 规定的 /bot<TOKEN>/<method> 地址。
func (a *HTTPAPI) methodURL(method string) string {
	return a.baseURL + "/bot" + a.token + "/" + method
}

// redact 移除任何由 Transport、自建 API 或请求构造错误回显的 Bot Token。
func (a *HTTPAPI) redact(value string) string {
	return strings.ReplaceAll(value, a.token, "[redacted]")
}

// safeFilename 防止受 Renderer 控制的文件名把路径或换行带入 multipart Header。
func safeFilename(value string) string {
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	value = filepath.Base(strings.TrimSpace(value))
	if value == "" || value == "." {
		return "smalux-speedtest.png"
	}
	return value
}
