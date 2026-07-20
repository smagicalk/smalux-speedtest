package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"time"

	"smalux-speedtest/internal/model"
)

// scanner 抽象 sql.Row 与 sql.Rows 共有的 Scan 方法，供单行和列表查询复用。
type scanner interface {
	Scan(dest ...any) error
}

// scanClient 将 SQLite 表示转换为公开 Client 结构。
// SQLite 使用 INTEGER 表示布尔值；labels_json 损坏时保留空标签，但其他字段扫描失败
// 会直接返回错误。查询语句必须严格遵循该函数定义的列顺序。
func scanClient(row scanner) (Client, error) {
	var client Client
	var labelsJSON string
	var enabled int
	err := row.Scan(&client.ID, &client.Name, &labelsJSON, &client.Version, &client.OS, &client.Arch, &enabled, &client.LastSeen, &client.CreatedAt)
	if err != nil {
		return Client{}, err
	}
	client.Enabled = enabled != 0
	_ = json.Unmarshal([]byte(labelsJSON), &client.Labels)
	// Read-side normalization ensures management APIs and reports remain safe even if
	// a legacy migration was interrupted or the database was modified out of band.
	client.Name = model.NormalizeClientName(client.Name)
	client.Labels = model.NormalizeClientLabels(client.Labels)
	client.Version = normalizeClientVersion(client.Version)
	client.OS = normalizeClientOS(client.OS)
	client.Arch = normalizeClientArch(client.Arch)
	return client, nil
}

// randomToken 使用密码学安全随机源生成 size 字节令牌，并采用无填充 Base64URL 编码。
// 该编码只含 URL/HTTP 头安全字符，便于直接作为 Client Bearer 凭据传递。
func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// tokenHash 返回 Client 令牌的稳定 SHA-256 十六进制摘要。
// 稳定摘要支持带索引的等值查找，同时避免在数据库中保存可直接使用的明文凭据。
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// now 统一生成 UTC RFC3339Nano 时间文本。
// 固定 UTC 避免时区混用；RFC3339Nano 会省略多余小数零，并非严格定长格式。当前
// 数据均由此函数生成，若未来需要混合外部时间格式做严格排序，应改存 Unix 时间戳。
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
