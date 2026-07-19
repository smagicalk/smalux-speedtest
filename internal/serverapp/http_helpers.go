package serverapp

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// decodeJSON 在 6 MiB 上限内解码单个 JSON 请求体，并拒绝未知字段。
// 限制体积可控制内存占用，拒绝未知字段则能尽早暴露前后端字段拼写或版本不一致。
func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 6<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// writeJSON 写入统一 JSON Content-Type、状态码和响应体。
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeError 使用统一 {"error":"..."} 结构返回错误，便于前端和 API 调用方处理。
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// requestIsTLS 判断客户端原始请求是否使用 HTTPS。
// X-Forwarded-Proto 用于服务部署在可信 TLS 终止反向代理之后的场景；部署方必须覆盖
// 外部传入的同名 Header，防止不可信客户端伪造代理信息。
func requestIsTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// csvCell 阻止用户可控文本被 Excel 等表格软件解释成公式。
// 在危险前缀前添加单引号只影响展示/解释方式，不会执行公式内容。
func csvCell(value string) string {
	if value != "" && strings.ContainsRune("=+-@", rune(value[0])) {
		return "'" + value
	}
	return value
}

// formatFloat 以固定两位小数输出测速数值，保证 CSV 格式稳定且便于比较。
func formatFloat(value float64) string { return strconv.FormatFloat(value, 'f', 2, 64) }
