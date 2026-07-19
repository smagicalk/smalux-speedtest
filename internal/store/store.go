package store

import (
	"context"
	"database/sql"

	_ "modernc.org/sqlite"
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
