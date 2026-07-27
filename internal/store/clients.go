package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"smalux-speedtest/internal/model"
)

// CreateClient 创建 Client 身份并返回公开元数据和仅展示一次的明文令牌。
//
// 令牌由 crypto/rand 生成 32 字节随机值，再编码成不需要 URL 转义的 Base64URL。
// 数据库只保存 SHA-256 哈希，后续无法从数据库恢复明文令牌。这里使用快速哈希是
// 因为令牌本身具有 256 位随机熵，不依赖人类密码的低熵特征；管理员密码仍使用 bcrypt。
func (s *Store) CreateClient(ctx context.Context, name string, labels map[string]string) (Client, string, error) {
	return s.CreateClientForAdmin(ctx, name, labels, "")
}

// CreateClientForAdmin 记录创建者，用于普通管理员只能修改自己 Client 的授权边界。
func (s *Store) CreateClientForAdmin(ctx context.Context, name string, labels map[string]string, ownerAdminID string) (Client, string, error) {
	if strings.TrimSpace(name) == "" {
		return Client{}, "", errors.New("client name is required")
	}
	client := Client{
		ID: model.NewID(), OwnerAdminID: strings.TrimSpace(ownerAdminID), Name: model.NormalizeClientName(name), Labels: model.NormalizeClientLabels(labels),
		Enabled: true, CreatedAt: now(),
	}
	secret, err := randomToken(32)
	if err != nil {
		return Client{}, "", err
	}
	token := secret
	labelsJSON, _ := json.Marshal(client.Labels)
	_, err = s.db.ExecContext(ctx, `INSERT INTO clients(id,owner_admin_id,name,token_hash,labels_json,created_at) VALUES(?,?,?,?,?,?)`,
		client.ID, client.OwnerAdminID, client.Name, tokenHash(token), string(labelsJSON), client.CreatedAt)
	return client, token, err
}

// RotateClientToken replaces an enabled Client's credential while preserving its
// stable identity and historical task associations. As with creation, only the
// one-time plaintext token is returned; persistence retains only its hash.
func (s *Store) RotateClientToken(ctx context.Context, id string) (string, error) {
	secret, err := randomToken(32)
	if err != nil {
		return "", err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE clients SET token_hash=? WHERE id=? AND enabled=1`, tokenHash(secret), id)
	if err != nil {
		return "", err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if changed == 0 {
		return "", sql.ErrNoRows
	}
	return secret, nil
}

// AuthenticateClient 按令牌哈希查找 Client，并同时检查是否已撤销。
// 返回的 Client 不包含 token_hash；空令牌和未知令牌都不会触发全表扫描。
func (s *Store) AuthenticateClient(ctx context.Context, token string) (Client, error) {
	if token == "" {
		return Client{}, errors.New("missing token")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id,owner_admin_id,name,labels_json,version,os,arch,enabled,last_seen,created_at FROM clients WHERE token_hash=?`, tokenHash(token))
	client, err := scanClient(row)
	if err != nil {
		return Client{}, errors.New("invalid client token")
	}
	if !client.Enabled {
		return Client{}, errors.New("client token is revoked")
	}
	return client, nil
}

// TouchClient 刷新 Client 的最后在线时间，通常由心跳处理调用。
func (s *Store) TouchClient(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE clients SET last_seen=? WHERE id=?`, now(), id)
	return err
}

// RevokeClient 将凭据标记为禁用而不删除历史数据。
// 保留 Client 行可以继续满足历史结果的外键与展示名称查询。
func (s *Store) RevokeClient(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE clients SET enabled=0 WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateClientMetadata 修改管理员维护的名称和标签，不触碰 Token 或运行时平台信息。
func (s *Store) UpdateClientMetadata(ctx context.Context, id, name string, labels map[string]string) (Client, error) {
	if strings.TrimSpace(name) == "" {
		return Client{}, errors.New("client name is required")
	}
	normalizedName := model.NormalizeClientName(name)
	normalizedLabels := model.NormalizeClientLabels(labels)
	labelsJSON, _ := json.Marshal(normalizedLabels)
	result, err := s.db.ExecContext(ctx, `UPDATE clients SET name=?,labels_json=? WHERE id=? AND enabled=1`, normalizedName, string(labelsJSON), id)
	if err != nil {
		return Client{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Client{}, err
	}
	if changed == 0 {
		return Client{}, sql.ErrNoRows
	}
	row := s.db.QueryRowContext(ctx, `SELECT id,owner_admin_id,name,labels_json,version,os,arch,enabled,last_seen,created_at FROM clients WHERE id=?`, id)
	return scanClient(row)
}

// ClientOwner 返回 Client 创建者；空值表示升级前创建、仅最高权限可管理。
func (s *Store) ClientOwner(ctx context.Context, id string) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT owner_admin_id FROM clients WHERE id=? AND enabled=1`, id).Scan(&owner)
	return owner, err
}

// ListClients 返回全部 Client 元数据，包含已撤销行；任务校验和历史关联需要这份
// 完整快照。查询明确不包含 token_hash。
func (s *Store) ListClients(ctx context.Context) ([]Client, error) {
	return s.listClients(ctx, ``)
}

// ListEnabledClients 只返回仍可用于新任务和管理页面展示的 Client。
// 撤销操作保留数据库行以满足历史结果外键，因此调用方若只需要当前可用节点，
// 必须使用本方法而不是在 UI 层猜测 enabled 字段。
func (s *Store) ListEnabledClients(ctx context.Context) ([]Client, error) {
	return s.listClients(ctx, ` WHERE enabled=1`)
}

// listClients 共享 Client 元数据扫描逻辑；whereClause 只由本文件中的固定常量传入，
// 不接受外部输入，避免把筛选条件拼接成 SQL 注入入口。
func (s *Store) listClients(ctx context.Context, whereClause string) ([]Client, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,owner_admin_id,name,labels_json,version,os,arch,enabled,last_seen,created_at FROM clients`+whereClause+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var clients []Client
	for rows.Next() {
		client, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		clients = append(clients, client)
	}
	return clients, rows.Err()
}
