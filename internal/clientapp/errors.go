package clientapp

import (
	"context"
	"errors"
	"fmt"

	"smalux-speedtest/internal/model"
)

var (
	errProxyInitialization  = errors.New("proxy initialization")
	errSpeedServerDiscovery = errors.New("speedtest server discovery")
	errNoSpeedServer        = errors.New("no speedtest server")
	errLatencyChecks        = errors.New("speedtest latency checks")
)

// executionError 保留底层错误供本地控制流使用，但调用方只能把
// executionResultError 返回的固定类别写入 SpeedResult。
func executionError(stage, cause error) error {
	return fmt.Errorf("%w: %w", stage, cause)
}

// executionResultError 将代理级错误映射为不含节点地址或认证材料的协议固定文本。
// 检查 context 原因优先于阶段原因，避免超时被误报成普通初始化/发现失败。
func executionResultError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return model.ResultErrorProxyTest
	case errors.Is(err, errProxyInitialization):
		return model.ResultErrorProxyInitialization
	case errors.Is(err, errSpeedServerDiscovery):
		return model.ResultErrorSpeedServerDiscovery
	case errors.Is(err, errNoSpeedServer):
		return model.ResultErrorNoSpeedServer
	case errors.Is(err, errLatencyChecks):
		return model.ResultErrorLatencyChecks
	default:
		return model.ResultErrorProxyTest
	}
}

// transferResultError 只公开失败方向，不保留 speedtest-go、目标 URL 或网络栈错误原文。
func transferResultError(downloadErr, uploadErr error) string {
	switch {
	case downloadErr != nil && uploadErr != nil:
		return model.ResultErrorTransfer
	case downloadErr != nil:
		return model.ResultErrorDownload
	case uploadErr != nil:
		return model.ResultErrorUpload
	default:
		return ""
	}
}
