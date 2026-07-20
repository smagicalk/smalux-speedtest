package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"smalux-speedtest/internal/model"
)

// TestStoreClientTaskAndResult 覆盖管理员初始化、Client 凭据签发与撤销、任务及目标的
// 事务创建、默认线程数，以及结果落库和联表读取的完整持久化流程。
func TestStoreClientTaskAndResult(t *testing.T) {
	ctx := context.Background()
	// 每个测试使用独立临时文件，既验证真实 SQLite 文件行为，也避免用例间共享状态。
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.BootstrapAdmin(ctx, "test-password"); err != nil {
		t.Fatal(err)
	}
	if !database.VerifyAdmin(ctx, "test-password") || database.VerifyAdmin(ctx, "wrong-password") {
		t.Fatal("admin password verification failed")
	}
	// CreateClient 返回的明文 token 应能认证，但 API 可见的 Client 只有公开元数据。
	client, token, err := database.CreateClient(ctx, "client-1", map[string]string{"region": "test"})
	if err != nil {
		t.Fatal(err)
	}
	authenticated, err := database.AuthenticateClient(ctx, token)
	if err != nil || authenticated.ID != client.ID {
		t.Fatalf("client authentication failed: %+v %v", authenticated, err)
	}
	// Threads 留为零，验证 Go 层默认值与数据库 schema 默认值均为 4。
	task := Task{ID: model.NewID(), Status: "queued", CandidateCount: 10, TopN: 3, ProxyCount: 1, ClientCount: 1, CreatedAt: now()}
	if err := database.CreateTask(ctx, task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	// 只落库已脱敏地址，随后检查速度值和 JOIN 得到的 ClientName 均能完整返回。
	result := model.SpeedResult{
		TaskID: task.ID, ClientID: client.ID, ProxyID: "proxy-1", ProxyName: "node", Protocol: "vless",
		MaskedAddress: "*.example.com:443", SpeedServerID: "123", LatencyMS: 10, DownloadBPS: 100_000_000,
	}
	if err := database.SaveResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	results, err := database.ListResults(ctx, task.ID)
	if err != nil || len(results) != 1 || results[0].DownloadBPS != result.DownloadBPS || results[0].ClientName != client.Name {
		t.Fatalf("unexpected results: %+v %v", results, err)
	}
	storedTask, err := database.GetTask(ctx, task.ID)
	if err != nil || storedTask.Threads != 4 {
		t.Fatalf("unexpected default thread count: %+v %v", storedTask, err)
	}
	if err := database.RevokeClient(ctx, client.ID); err != nil {
		t.Fatal(err)
	}
	activeClients, err := database.ListEnabledClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(activeClients) != 0 {
		t.Fatalf("revoked client remained in enabled list: %+v", activeClients)
	}
	allClients, err := database.ListClients(ctx)
	if err != nil || len(allClients) != 1 || allClients[0].Enabled {
		t.Fatalf("revoked client history row was unexpectedly removed: %+v, %v", allClients, err)
	}
	if _, err := database.AuthenticateClient(ctx, token); err == nil {
		t.Fatal("revoked token was accepted")
	}
	// Upgrade 前已经通过认证的连接仍可能在撤销后发送 hello；条件更新必须拒绝它。
	if err := database.UpdateClientHello(ctx, client.ID, model.Hello{Name: "late-client"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked client updated hello: %v", err)
	}
	// CreateTask 在自己的事务中再次检查 Enabled，堵住服务层校验与提交之间的竞态；
	// 失败事务也不能留下没有目标的父任务。
	revokedTask := Task{ID: model.NewID(), Status: "queued", CandidateCount: 1, TopN: 1, ProxyCount: 1, CreatedAt: now()}
	if err := database.CreateTask(ctx, revokedTask, []string{client.ID}); err == nil {
		t.Fatal("task was created for a revoked client")
	}
	if _, err := database.GetTask(ctx, revokedTask.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed task transaction left parent row: %v", err)
	}
}

// TestOpenMigratesTaskThreads 通过不含 threads 列的旧表验证启动迁移会原地补列，并让
// 历史行获得 schema 指定的默认值 4。
func TestOpenMigratesTaskThreads(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY, status TEXT NOT NULL, candidate_count INTEGER NOT NULL, top_n INTEGER NOT NULL,
		proxy_count INTEGER NOT NULL, client_count INTEGER NOT NULL, error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, started_at TEXT NOT NULL DEFAULT '', finished_at TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := now()
	if _, err := legacy.Exec(`INSERT INTO tasks(id,status,candidate_count,top_n,proxy_count,client_count,created_at) VALUES('legacy','completed',10,3,1,1,?)`, createdAt); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新通过正式入口打开，确保测试的是启动时迁移而非新库建表路径。
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	task, err := database.GetTask(ctx, "legacy")
	if err != nil || task.Threads != 4 {
		t.Fatalf("legacy task was not migrated: %+v %v", task, err)
	}
}

// TestOpenMarksInterruptedTasksFailed 用两次 Open 模拟进程重启，确认无法恢复内存代理
// 配置的 running 任务会明确转为 failed，而不会永久停留在运行态。
func TestOpenMarksInterruptedTasksFailed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.BootstrapAdmin(ctx, "test-password"); err != nil {
		t.Fatal(err)
	}
	client, _, err := first.CreateClient(ctx, "client", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: model.NewID(), Status: "running", CandidateCount: 10, TopN: 1, ProxyCount: 1, CreatedAt: now()}
	if err := first.CreateTask(ctx, task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	first.Close()
	// 第二次 Open 会执行幂等迁移及启动恢复逻辑。
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	stored, err := second.GetTask(ctx, task.ID)
	if err != nil || stored.Status != "failed" {
		t.Fatalf("unexpected task after restart: %+v %v", stored, err)
	}
	completed, failed, canceled, total, err := second.TargetSummary(ctx, task.ID)
	if err != nil || completed != 0 || failed != 1 || canceled != 0 || total != 1 {
		t.Fatalf("interrupted targets were not failed: completed=%d failed=%d canceled=%d total=%d err=%v", completed, failed, canceled, total, err)
	}
}
