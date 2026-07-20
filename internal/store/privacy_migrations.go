package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"smalux-speedtest/internal/model"
)

const (
	privacySensitiveScrubSetting = "privacy_sensitive_scrub_v2"
	privacyProxyNameScrubSetting = "privacy_proxy_name_scrub_v3"
	privacyClientScrubSetting    = "privacy_client_metadata_scrub_v4"
	privacyTimestampScrubSetting = "privacy_timestamp_scrub_v6"
	privacyStoragePurgeSetting   = "privacy_storage_purge_v7"
	legacySpeedServerName        = "Speedtest.net"
)

// scrubLegacyClientMetadata removes persistence channels that existed before the
// administrator-owned Client identity boundary. Older Hello handling allowed a bearer
// holder to overwrite name, labels and runtime fields with arbitrary text; ordinary
// administrator labels survive, while endpoint/path/credential-shaped values do not.
func (s *Store) scrubLegacyClientMetadata(ctx context.Context) error {
	var marker string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, privacyClientScrubSetting).Scan(&marker)
	if err == nil && marker == "complete" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read Client metadata scrub marker: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	type clientUpdate struct {
		id, name, labels, version, os, arch string
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,name,labels_json,version,os,arch FROM clients`)
	if err != nil {
		return fmt.Errorf("read legacy Client metadata: %w", err)
	}
	var updates []clientUpdate
	for rows.Next() {
		var id, name, labelsJSON, version, operatingSystem, architecture string
		if err := rows.Scan(&id, &name, &labelsJSON, &version, &operatingSystem, &architecture); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy Client metadata: %w", err)
		}
		var labels map[string]string
		_ = json.Unmarshal([]byte(labelsJSON), &labels)
		normalizedLabels, marshalErr := json.Marshal(model.NormalizeClientLabels(labels))
		if marshalErr != nil {
			rows.Close()
			return fmt.Errorf("encode scrubbed Client labels: %w", marshalErr)
		}
		updates = append(updates, clientUpdate{
			id: id, name: model.NormalizeClientName(name), labels: string(normalizedLabels),
			version: normalizeClientVersion(version), os: normalizeClientOS(operatingSystem), arch: normalizeClientArch(architecture),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate legacy Client metadata: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy Client metadata: %w", err)
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE clients SET name=?,labels_json=?,version=?,os=?,arch=? WHERE id=?`,
			update.name, update.labels, update.version, update.os, update.arch, update.id); err != nil {
			return fmt.Errorf("scrub legacy Client metadata: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, privacyClientScrubSetting, "complete"); err != nil {
		return fmt.Errorf("write Client metadata scrub marker: %w", err)
	}
	return tx.Commit()
}

// scrubLegacySensitiveData 对升级前可能保存的敏感结果字段执行一次性清洗。
//
// 新版写路径只接受固定错误类别，但旧数据库中的 results/tasks/task_targets.error 可能
// 已经包含 sing-box、网络库或异常 Client 的原文。旧版 masked_address 还保留地址前缀，
// 异常 Client 也可能借 Speedtest 元数据夹带配置。该 v2 事务清除结果的其他不必要
// 文本；proxy_name 由独立 v3 迁移处理，确保已经执行过 v2 的数据库也能升级。
func (s *Store) scrubLegacySensitiveData(ctx context.Context) error {
	var marker string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, privacySensitiveScrubSetting).Scan(&marker)
	if err == nil && marker == "complete" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read privacy migration marker: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// id 是服务端生成的随机结果主键。取其前 16 个十六进制字符作为匿名 server
	// ID，既保持每行在旧数据中的近似唯一性，又符合当前 server-<16 hex> 形态；
	// 不把任何旧 Speedtest ID 或 Host 原文带入新值。
	if _, err := tx.ExecContext(ctx, `UPDATE results SET
		masked_address='[redacted]',
		speed_server_id='server-' || substr(lower(id),1,16),
		speed_server_name=?,speed_server_host='',country='',sponsor=''`, legacySpeedServerName); err != nil {
		return fmt.Errorf("scrub legacy result metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE results SET error=? WHERE error<>''`, model.ResultErrorProxyTest); err != nil {
		return fmt.Errorf("scrub legacy result errors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_targets SET error=? WHERE error<>''`, model.TaskFailureClient); err != nil {
		return fmt.Errorf("scrub legacy target errors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET error=? WHERE error<>''`, model.TaskFailureClient); err != nil {
		return fmt.Errorf("scrub legacy task errors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, privacySensitiveScrubSetting, "complete"); err != nil {
		return fmt.Errorf("write privacy migration marker: %w", err)
	}
	return tx.Commit()
}

// scrubLegacyProxyNames is a one-time v3 data migration for result names written by
// older importers. It rewrites only names that match the shared sensitive-value rules;
// normal labels remain unchanged. The v5 physical rewrite below follows this logical
// update to remove obsolete values from local database and WAL pages.
func (s *Store) scrubLegacyProxyNames(ctx context.Context) error {
	var marker string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, privacyProxyNameScrubSetting).Scan(&marker)
	if err == nil && marker == "complete" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read proxy-name migration marker: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	type nameUpdate struct {
		id   string
		name string
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,protocol,proxy_name FROM results`)
	if err != nil {
		return fmt.Errorf("read legacy proxy names: %w", err)
	}
	var updates []nameUpdate
	for rows.Next() {
		var id, protocol, name string
		if err := rows.Scan(&id, &protocol, &name); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy proxy name: %w", err)
		}
		normalized := model.NormalizeProxyName(protocol, name, "")
		if normalized != name {
			updates = append(updates, nameUpdate{id: id, name: normalized})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate legacy proxy names: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy proxy names: %w", err)
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE results SET proxy_name=? WHERE id=?`, update.name, update.id); err != nil {
			return fmt.Errorf("scrub legacy proxy name: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, privacyProxyNameScrubSetting, "complete"); err != nil {
		return fmt.Errorf("write proxy-name migration marker: %w", err)
	}
	return tx.Commit()
}

// purgeLegacyStorage performs a one-time physical rewrite after the logical scrubs
// finish. secure_delete covers records changed by those migrations; VACUUM rebuilds
// database pages and the TRUNCATE checkpoints remove obsolete WAL frames. This cannot
// affect copies outside the configured database path, such as operator backups.
func (s *Store) purgeLegacyStorage(ctx context.Context) error {
	var marker string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, privacyStoragePurgeSetting).Scan(&marker)
	if err == nil && marker == "complete" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read storage purge marker: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("rewrite database after privacy scrub: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("truncate privacy scrub WAL: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, privacyStoragePurgeSetting, "complete"); err != nil {
		return fmt.Errorf("write storage purge marker: %w", err)
	}
	// The marker contains no user data, but checkpoint it as well so a clean startup
	// does not leave an otherwise unnecessary WAL file behind.
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint storage purge marker: %w", err)
	}
	return nil
}
