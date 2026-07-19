package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"golang.org/x/crypto/bcrypt"

	"smalux-speedtest/internal/model"
)

// BootstrapAdmin 在数据库尚未初始化管理员密码时写入 bcrypt 哈希。
//
// 已存在哈希时本函数不覆盖它，因此环境变量只负责首次启动，不会在普通重启时
// 静默修改密码。数据库从不保存管理员明文密码。
func (s *Store) BootstrapAdmin(ctx context.Context, password string) error {
	var existing string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='admin_password_hash'`).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(password) < 8 {
		return errors.New("SMALUX_ADMIN_PASSWORD must contain at least 8 characters on first start")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('admin_password_hash',?)`, string(hash))
	return err
}

// VerifyAdmin 使用 bcrypt 验证管理员密码。
// 查询失败与密码错误都统一返回 false，避免上层根据数据库内容暴露认证细节。
func (s *Store) VerifyAdmin(ctx context.Context, password string) bool {
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='admin_password_hash'`).Scan(&hash); err != nil {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// CreateClient 创建 Client 身份并返回公开元数据和仅展示一次的明文令牌。
//
// 令牌由 crypto/rand 生成 32 字节随机值，再编码成不需要 URL 转义的 Base64URL。
// 数据库只保存 SHA-256 哈希，后续无法从数据库恢复明文令牌。这里使用快速哈希是
// 因为令牌本身具有 256 位随机熵，不依赖人类密码的低熵特征；管理员密码仍使用 bcrypt。
func (s *Store) CreateClient(ctx context.Context, name string, labels map[string]string) (Client, string, error) {
	if name == "" {
		return Client{}, "", errors.New("client name is required")
	}
	token, err := randomToken(32)
	if err != nil {
		return Client{}, "", err
	}
	client := Client{ID: model.NewID(), Name: name, Labels: labels, Enabled: true, CreatedAt: now()}
	labelsJSON, _ := json.Marshal(labels)
	_, err = s.db.ExecContext(ctx, `INSERT INTO clients(id,name,token_hash,labels_json,created_at) VALUES(?,?,?,?,?)`,
		client.ID, client.Name, tokenHash(token), string(labelsJSON), client.CreatedAt)
	return client, token, err
}

// AuthenticateClient 按令牌哈希查找 Client，并同时检查是否已撤销。
// 返回的 Client 不包含 token_hash；空令牌和未知令牌都不会触发全表扫描。
func (s *Store) AuthenticateClient(ctx context.Context, token string) (Client, error) {
	if token == "" {
		return Client{}, errors.New("missing token")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id,name,labels_json,version,os,arch,enabled,last_seen,created_at FROM clients WHERE token_hash=?`, tokenHash(token))
	client, err := scanClient(row)
	if err != nil {
		return Client{}, errors.New("invalid client token")
	}
	if !client.Enabled {
		return Client{}, errors.New("client token is revoked")
	}
	return client, nil
}

// UpdateClientHello 保存 Client 最近一次握手上报的名称、标签、版本和运行平台。
// 此更新不触碰身份令牌及启用状态，但只允许仍启用的 Client 更新。WebSocket 在
// Upgrade 后再次调用本方法，可堵住“初次认证成功、随后凭据被撤销”的握手窗口。
func (s *Store) UpdateClientHello(ctx context.Context, id string, hello model.Hello) error {
	labels, _ := json.Marshal(hello.Labels)
	result, err := s.db.ExecContext(ctx, `UPDATE clients SET name=?,labels_json=?,version=?,os=?,arch=?,last_seen=? WHERE id=? AND enabled=1`,
		hello.Name, string(labels), hello.Version, hello.OS, hello.Arch, now(), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
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

// ListClients 返回管理界面可见的 Client 元数据，明确不查询 token_hash。
func (s *Store) ListClients(ctx context.Context) ([]Client, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,labels_json,version,os,arch,enabled,last_seen,created_at FROM clients ORDER BY name`)
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
