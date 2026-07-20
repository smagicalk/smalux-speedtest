package store

import (
	"context"
	"database/sql"
	"fmt"

	"smalux-speedtest/internal/model"
)

// SaveResult 幂等保存一个“任务 + Client + 代理 + 测速服务器”的结果。
//
// 唯一键冲突表示 Client 重发同一结果，此时仅更新测量值、错误与时间，而保留原始行
// 的身份字段。MaskedAddress 应在调用本方法前完成脱敏；该层不会接触也不会保存代理
// 密码、UUID、分享链接或 outbound JSON。
func (s *Store) SaveResult(ctx context.Context, result model.SpeedResult) error {
	// Client timestamps are untrusted and can be stale, forged or contain a
	// sentinel. The service assigns the only persisted result timestamp.
	result.CreatedAt = now()
	// Store 是最后一道持久化边界。即使未来新增调用方绕过 Hub 校验，也不能把任意
	// Client 错误、代理地址、测速 Host 或原始服务器 ID 写入 SQLite。
	result.Protocol = model.NormalizeResultProtocol(result.Protocol)
	result.ProxyID = normalizePersistedProxyID(result.ProxyID)
	result.ProxyName = model.NormalizeProxyName(result.Protocol, result.ProxyName, "")
	result.Error = model.NormalizeResultError(result.Error)
	result.MaskedAddress = normalizePersistedAddress(result.MaskedAddress)
	result.SpeedServerID = normalizePersistedSpeedServerID(result.SpeedServerID)
	result.SpeedServerHost = ""
	result.SpeedServerName = model.NormalizePublicResultLabel(result.SpeedServerName, 256)
	result.Country = model.NormalizePublicResultLabel(result.Country, 64)
	result.Sponsor = model.NormalizePublicResultLabel(result.Sponsor, 256)
	_, err := s.db.ExecContext(ctx, `INSERT INTO results(
		id,task_id,client_id,proxy_id,proxy_name,protocol,masked_address,speed_server_id,speed_server_name,speed_server_host,country,sponsor,
		latency_ms,jitter_ms,download_bps,upload_bps,duration_ms,error,created_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(task_id,client_id,proxy_id,speed_server_id) DO UPDATE SET
		latency_ms=excluded.latency_ms,jitter_ms=excluded.jitter_ms,download_bps=excluded.download_bps,upload_bps=excluded.upload_bps,
		duration_ms=excluded.duration_ms,error=excluded.error,created_at=excluded.created_at`,
		model.NewID(), result.TaskID, result.ClientID, result.ProxyID, result.ProxyName, result.Protocol, result.MaskedAddress,
		result.SpeedServerID, result.SpeedServerName, result.SpeedServerHost, result.Country, result.Sponsor,
		result.LatencyMS, result.JitterMS, result.DownloadBPS, result.UploadBPS, result.DurationMS, result.Error, result.CreatedAt)
	return err
}

// ListResults 返回任务的全部测速结果，并联表补充当前 Client 名称。
// 查询字段只包含 masked_address，不包含任何代理认证材料。Client 被撤销后行仍保留，
// 因此历史结果仍可展示；名称取当前值而非执行任务时的快照。
func (s *Store) ListResults(ctx context.Context, taskID string) ([]model.SpeedResult, error) {
	return s.listResults(ctx, taskID, 0)
}

// ListResultsLimited 为图片等有固定展示上限的调用方只读取前 limit 条。
// limit 必须在 1-500 内，防止调用方误传大值绕过内存边界。
func (s *Store) ListResultsLimited(ctx context.Context, taskID string, limit int) ([]model.SpeedResult, error) {
	if limit < 1 || limit > 500 {
		return nil, fmt.Errorf("result limit must be between 1 and 500")
	}
	return s.listResults(ctx, taskID, limit)
}

// CountResults 返回任务完整结果数，使图片页脚能显示“前 N 条/共 M 条”，
// 而不必先把 M 条结果全部扫描到 Go 内存。
func (s *Store) CountResults(ctx context.Context, taskID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM results WHERE task_id=?`, taskID).Scan(&count)
	return count, err
}

func (s *Store) listResults(ctx context.Context, taskID string, limit int) ([]model.SpeedResult, error) {
	query := `SELECT r.task_id,r.client_id,c.name,r.proxy_id,r.proxy_name,r.protocol,r.masked_address,r.speed_server_id,r.speed_server_name,r.speed_server_host,r.country,r.sponsor,
		r.latency_ms,r.jitter_ms,r.download_bps,r.upload_bps,r.duration_ms,r.error,r.created_at FROM results r JOIN clients c ON c.id=r.client_id WHERE r.task_id=? ORDER BY r.proxy_name,r.proxy_id,c.name,r.client_id,r.latency_ms`
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.QueryContext(ctx, query+` LIMIT ?`, taskID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, query, taskID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []model.SpeedResult
	for rows.Next() {
		var result model.SpeedResult
		if err := rows.Scan(&result.TaskID, &result.ClientID, &result.ClientName, &result.ProxyID, &result.ProxyName, &result.Protocol, &result.MaskedAddress,
			&result.SpeedServerID, &result.SpeedServerName, &result.SpeedServerHost, &result.Country, &result.Sponsor,
			&result.LatencyMS, &result.JitterMS, &result.DownloadBPS, &result.UploadBPS, &result.DurationMS, &result.Error, &result.CreatedAt); err != nil {
			return nil, err
		}
		// Read-side normalization protects callers if a legacy database is opened
		// read-only or migration was interrupted before its marker was written.
		result.ClientName = model.NormalizeClientName(result.ClientName)
		result.Protocol = model.NormalizeResultProtocol(result.Protocol)
		result.ProxyID = normalizePersistedProxyID(result.ProxyID)
		result.ProxyName = model.NormalizeProxyName(result.Protocol, result.ProxyName, "")
		result.MaskedAddress = normalizePersistedAddress(result.MaskedAddress)
		result.SpeedServerID = normalizePersistedSpeedServerID(result.SpeedServerID)
		result.SpeedServerHost = ""
		result.SpeedServerName = model.NormalizePublicResultLabel(result.SpeedServerName, 256)
		result.Country = model.NormalizePublicResultLabel(result.Country, 64)
		result.Sponsor = model.NormalizePublicResultLabel(result.Sponsor, 256)
		result.Error = model.NormalizeResultError(result.Error)
		result.CreatedAt = normalizePersistedTimestamp(result.CreatedAt)
		results = append(results, result)
	}
	return results, rows.Err()
}
