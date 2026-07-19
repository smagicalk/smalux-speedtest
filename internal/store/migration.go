package store

import (
	"context"
	"fmt"
)

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
		// telegram_users 是 Telegram Bot 的授权白名单。owner 由服务启动配置同步，
		// 普通授权只允许管理 is_owner=0 的行；用户名仅用于展示，身份判断始终使用 ID。
		`CREATE TABLE IF NOT EXISTS telegram_users (
			telegram_id INTEGER PRIMARY KEY CHECK(telegram_id > 0),
			username TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			is_owner INTEGER NOT NULL DEFAULT 0 CHECK(is_owner IN (0,1)),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		// 部分唯一索引让 is_owner=0 的普通用户不受限制，同时从 schema 层保证 owner 唯一。
		`CREATE UNIQUE INDEX IF NOT EXISTS telegram_users_owner_idx ON telegram_users(is_owner) WHERE is_owner=1`,
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
	// 先终结这些任务的活动目标，再更新父任务，避免详情页在重启后仍显示
	// queued/running Client。迁移在服务开放请求前执行，此时不存在并发读写。
	const restartDetail = "server restarted before task completed"
	if _, err := s.db.ExecContext(ctx, `UPDATE task_targets SET status='failed',error=?
		WHERE status IN ('queued','running') AND EXISTS(
			SELECT 1 FROM tasks WHERE tasks.id=task_targets.task_id AND tasks.status IN ('queued','running'))`, restartDetail); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='failed', error=?, finished_at=? WHERE status IN ('queued','running')`, restartDetail, now())
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
