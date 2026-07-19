package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"

	"smalux-speedtest/internal/model"
)

// Store 是服务端持久化存储的入口。
//
// db 不直接导出，调用方只能通过本包方法读写预定义的数据形态，从而避免 API 层
// 意外查询到 token_hash 等不应返回给前端的字段。
type Store struct {
	// db 是限制为单连接的 SQLite 连接池，所有公开方法共享该连接。
	db *sql.DB
}

// Client 是可以连接服务端并执行测速任务的客户端公开元数据。
// 它刻意不包含令牌或令牌哈希，因此可以安全地作为管理 API 的响应结构使用。
type Client struct {
	// ID 是服务端生成且长期稳定的 Client 标识。
	ID string `json:"id"`
	// Name 由管理员创建，Client 握手后可上报最新显示名称。
	Name string `json:"name"`
	// Labels 保存地区、线路或提供商等自由键值元数据。
	Labels map[string]string `json:"labels,omitempty"`
	// Version 来自最近一次 Hello 消息，用于识别客户端能力。
	Version string `json:"version,omitempty"`
	// OS 是最近一次握手上报的 Go 目标操作系统。
	OS string `json:"os,omitempty"`
	// Arch 是最近一次握手上报的 Go 目标架构。
	Arch string `json:"arch,omitempty"`
	// Enabled 为 false 表示令牌已撤销，后续认证必须拒绝。
	Enabled bool `json:"enabled"`
	// LastSeen 在握手、心跳或连接结束时更新。
	LastSeen string `json:"last_seen,omitempty"`
	// CreatedAt 是服务端创建该 Client 凭据的时间。
	CreatedAt string `json:"created_at"`
}

// Task 是一批“代理 x Client”测速工作的持久化摘要。
// 具体代理配置不写入数据库，这里只记录调度参数、数量和生命周期状态。
type Task struct {
	// ID 是任务的全局随机标识，也是 API、SSE 和 WebSocket 关联任务的主键。
	ID string `json:"id"`
	// Status 由调度器维护，通常依次经历 queued、running 和一个终态。
	Status string `json:"status"`
	// CandidateCount 是延迟探测候选测速服务器数。
	CandidateCount int `json:"candidate_count"`
	// TopN 是按延迟筛选后继续执行上下行测试的节点数量。
	TopN int `json:"top_n"`
	// Threads 是单个测速任务使用的并发连接数。
	Threads int `json:"threads"`
	// ProxyCount 是任务导入成功并参与调度的代理数量。
	ProxyCount int `json:"proxy_count"`
	// ClientCount 是该任务选择的目标 Client 数量。
	ClientCount int `json:"client_count"`
	// Error 保存任务级状态说明，不包含代理分享链接或连接凭据。
	Error string `json:"error,omitempty"`
	// CreatedAt 是任务创建时的 UTC RFC3339Nano 时间。
	CreatedAt string `json:"created_at"`
	// StartedAt 是任务首次进入 running 的 UTC RFC3339Nano 时间。
	StartedAt string `json:"started_at,omitempty"`
	// FinishedAt 是任务进入任一终态的 UTC RFC3339Nano 时间。
	FinishedAt string `json:"finished_at,omitempty"`
}

// Open 打开或创建 SQLite 数据库，并在返回前完成幂等 schema 迁移。
// 迁移失败时会立即关闭底层连接，调用方不会得到半初始化的 Store。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite 的 foreign_keys 和 busy_timeout 是连接级设置。固定单连接可确保 migrate
	// 执行的 PRAGMA 同样约束后续查询，并减少控制面并发写产生 SQLITE_BUSY 的概率。
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// Close 释放 SQLite 连接。
func (s *Store) Close() error { return s.db.Close() }

// migrate 创建当前版本所需的表和索引，并执行兼容旧数据库的增量迁移。
//
// 各语句均设计为可重复执行。这里没有将整个过程包进事务，因为 journal_mode 等
// PRAGMA 对事务上下文有额外限制；新增列则由 ensureColumn 先探测再执行 ALTER。
func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		// WAL 允许读取与写入更好地并行；单进程仍以一个连接串行访问数据库。
		`PRAGMA journal_mode=WAL`,
		// task_targets/results 的外键约束依赖此连接级开关。
		`PRAGMA foreign_keys=ON`,
		// 短暂写锁冲突时等待，而不是立即向 API 返回 SQLITE_BUSY。
		`PRAGMA busy_timeout=5000`,
		// settings 用于少量服务端配置，目前包含 bcrypt 管理员密码哈希。
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		// clients 仅保存不可逆 token_hash，明文令牌只在 CreateClient 返回一次。
		`CREATE TABLE IF NOT EXISTS clients (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			labels_json TEXT NOT NULL DEFAULT '{}',
			version TEXT NOT NULL DEFAULT '',
			os TEXT NOT NULL DEFAULT '',
			arch TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			last_seen TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		// tasks 只保存任务摘要，代理凭据不会随任务持久化。
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			candidate_count INTEGER NOT NULL,
			top_n INTEGER NOT NULL,
			threads INTEGER NOT NULL DEFAULT 4,
			proxy_count INTEGER NOT NULL,
			client_count INTEGER NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			started_at TEXT NOT NULL DEFAULT '',
			finished_at TEXT NOT NULL DEFAULT ''
		)`,
		// task_targets 将一个任务拆成每个 Client 的独立状态，联合主键防止重复下发。
		`CREATE TABLE IF NOT EXISTS task_targets (
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			client_id TEXT NOT NULL REFERENCES clients(id),
			status TEXT NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(task_id, client_id)
		)`,
		// results 保存可展示结果。masked_address 必须由上游先脱敏；此表没有原始地址列。
		// 末尾联合唯一键同时作为幂等键，使 Client 重发同一结果时更新而非追加重复行。
		`CREATE TABLE IF NOT EXISTS results (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			client_id TEXT NOT NULL REFERENCES clients(id),
			proxy_id TEXT NOT NULL,
			proxy_name TEXT NOT NULL,
			protocol TEXT NOT NULL,
			masked_address TEXT NOT NULL,
			speed_server_id TEXT NOT NULL DEFAULT '',
			speed_server_name TEXT NOT NULL DEFAULT '',
			speed_server_host TEXT NOT NULL DEFAULT '',
			country TEXT NOT NULL DEFAULT '',
			sponsor TEXT NOT NULL DEFAULT '',
			latency_ms REAL NOT NULL DEFAULT 0,
			jitter_ms REAL NOT NULL DEFAULT 0,
			download_bps REAL NOT NULL DEFAULT 0,
			upload_bps REAL NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			UNIQUE(task_id, client_id, proxy_id, speed_server_id)
		)`,
		`CREATE INDEX IF NOT EXISTS results_task_idx ON results(task_id)`,
		`CREATE INDEX IF NOT EXISTS tasks_created_idx ON tasks(created_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	}
	if err := s.ensureColumn(ctx, "tasks", "threads", "INTEGER NOT NULL DEFAULT 4"); err != nil {
		return err
	}
	// 任务的代理配置只存在内存中，进程重启后不可能可靠恢复 queued/running 任务。
	// 将其显式终结可避免管理界面永久显示“运行中”。已完成任务不受影响。
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='failed', error='server restarted before task completed', finished_at=? WHERE status IN ('queued','running')`, now())
	return err
}

// ensureColumn 为旧数据库补充新增列。
//
// table、column 和 definition 会拼接进 SQLite DDL，因此该辅助函数只允许使用本包内
// 编译期常量调用，绝不能传入 HTTP 参数等外部输入。SQLite 缺少通用的
// "ADD COLUMN IF NOT EXISTS"，所以先通过 table_info 判断列是否已存在。
func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var position, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&position, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+definition)
	return err
}

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
// 此更新不触碰身份令牌及启用状态。
func (s *Store) UpdateClientHello(ctx context.Context, id string, hello model.Hello) error {
	labels, _ := json.Marshal(hello.Labels)
	_, err := s.db.ExecContext(ctx, `UPDATE clients SET name=?,labels_json=?,version=?,os=?,arch=?,last_seen=? WHERE id=?`,
		hello.Name, string(labels), hello.Version, hello.OS, hello.Arch, now(), id)
	return err
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

// CreateTask 原子地写入任务摘要和全部目标 Client。
//
// tasks 与 task_targets 必须同时成功或同时失败，否则调度器可能看到一个没有目标的
// 任务，或看到引用不存在任务的目标。defer Rollback 在任意提前返回时清理事务；成功
// Commit 后再次 Rollback 是无害的。ClientCount 从 clientIDs 计算，避免信任调用方字段。
func (s *Store) CreateTask(ctx context.Context, task Task, clientIDs []string) error {
	// 兼容未显式设置线程数的旧调用方，并与 schema 的 DEFAULT 4 保持一致。
	if task.Threads == 0 {
		task.Threads = 4
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO tasks(id,status,candidate_count,top_n,threads,proxy_count,client_count,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		task.ID, task.Status, task.CandidateCount, task.TopN, task.Threads, task.ProxyCount, len(clientIDs), task.CreatedAt)
	if err != nil {
		return err
	}
	for _, clientID := range clientIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_targets(task_id,client_id,status) VALUES(?,?,'queued')`, task.ID, clientID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetTaskStatus 更新任务状态、说明及生命周期时间。
//
// started_at 只在第一次进入 running 且原值为空时写入；终态会写 finished_at。CASE
// 表达式使状态与时间在同一条 SQL 中更新，避免并发读取到不一致的中间状态。
func (s *Store) SetTaskStatus(ctx context.Context, id, status, detail string) error {
	startedAt := ""
	finishedAt := ""
	if status == "running" {
		startedAt = now()
	}
	if status == "completed" || status == "partial" || status == "failed" || status == "canceled" {
		finishedAt = now()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status=?,error=?,started_at=CASE WHEN started_at='' AND ?<>'' THEN ? ELSE started_at END,finished_at=CASE WHEN ?<>'' THEN ? ELSE finished_at END WHERE id=?`,
		status, detail, startedAt, startedAt, finishedAt, finishedAt, id)
	return err
}

// SetTargetStatus 更新任务中某个 Client 的独立执行状态与错误说明。
func (s *Store) SetTargetStatus(ctx context.Context, taskID, clientID, status, detail string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE task_targets SET status=?,error=? WHERE task_id=? AND client_id=?`, status, detail, taskID, clientID)
	return err
}

// TargetSummary 聚合任务各目标 Client 的终态数量。
// total 包含 queued/running 等所有状态；具名的 completed、failed、canceled 只统计对应
// 终态，调用方可据此判断任务最终应为 completed、partial 还是 failed。
func (s *Store) TargetSummary(ctx context.Context, taskID string) (completed, failed, canceled, total int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status,COUNT(*) FROM task_targets WHERE task_id=? GROUP BY status`, taskID)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return 0, 0, 0, 0, err
		}
		total += count
		switch status {
		case "completed":
			completed += count
		case "failed":
			failed += count
		case "canceled":
			canceled += count
		}
	}
	return completed, failed, canceled, total, rows.Err()
}

// SaveResult 幂等保存一个“任务 + Client + 代理 + 测速服务器”的结果。
//
// 唯一键冲突表示 Client 重发同一结果，此时仅更新测量值、错误与时间，而保留原始行
// 的身份字段。MaskedAddress 应在调用本方法前完成脱敏；该层不会接触也不会保存代理
// 密码、UUID、分享链接或 outbound JSON。
func (s *Store) SaveResult(ctx context.Context, result model.SpeedResult) error {
	if result.CreatedAt == "" {
		result.CreatedAt = now()
	}
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

// ListTasks 按创建时间倒序返回最近任务。
// limit 异常时回落到 50，并以 200 为硬上限，避免管理 API 一次读取无界历史数据。
func (s *Store) ListTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,status,candidate_count,top_n,threads,proxy_count,client_count,error,created_at,started_at,finished_at FROM tasks ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var task Task
		if err := rows.Scan(&task.ID, &task.Status, &task.CandidateCount, &task.TopN, &task.Threads, &task.ProxyCount, &task.ClientCount, &task.Error, &task.CreatedAt, &task.StartedAt, &task.FinishedAt); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// GetTask 按 ID 返回单个任务摘要；不存在时返回 sql.ErrNoRows。
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	var task Task
	err := s.db.QueryRowContext(ctx, `SELECT id,status,candidate_count,top_n,threads,proxy_count,client_count,error,created_at,started_at,finished_at FROM tasks WHERE id=?`, id).
		Scan(&task.ID, &task.Status, &task.CandidateCount, &task.TopN, &task.Threads, &task.ProxyCount, &task.ClientCount, &task.Error, &task.CreatedAt, &task.StartedAt, &task.FinishedAt)
	return task, err
}

// ListResults 返回任务的全部测速结果，并联表补充当前 Client 名称。
// 查询字段只包含 masked_address，不包含任何代理认证材料。Client 被撤销后行仍保留，
// 因此历史结果仍可展示；名称取当前值而非执行任务时的快照。
func (s *Store) ListResults(ctx context.Context, taskID string) ([]model.SpeedResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.task_id,r.client_id,c.name,r.proxy_id,r.proxy_name,r.protocol,r.masked_address,r.speed_server_id,r.speed_server_name,r.speed_server_host,r.country,r.sponsor,
		r.latency_ms,r.jitter_ms,r.download_bps,r.upload_bps,r.duration_ms,r.error,r.created_at FROM results r JOIN clients c ON c.id=r.client_id WHERE r.task_id=? ORDER BY r.proxy_name,r.latency_ms`, taskID)
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
		results = append(results, result)
	}
	return results, rows.Err()
}

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
