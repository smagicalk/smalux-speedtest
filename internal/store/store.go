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

type Store struct {
	db *sql.DB
}

type Client struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Version   string            `json:"version,omitempty"`
	OS        string            `json:"os,omitempty"`
	Arch      string            `json:"arch,omitempty"`
	Enabled   bool              `json:"enabled"`
	LastSeen  string            `json:"last_seen,omitempty"`
	CreatedAt string            `json:"created_at"`
}

type Task struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	CandidateCount int    `json:"candidate_count"`
	TopN           int    `json:"top_n"`
	Threads        int    `json:"threads"`
	ProxyCount     int    `json:"proxy_count"`
	ClientCount    int    `json:"client_count"`
	Error          string `json:"error,omitempty"`
	CreatedAt      string `json:"created_at"`
	StartedAt      string `json:"started_at,omitempty"`
	FinishedAt     string `json:"finished_at,omitempty"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
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
		`CREATE TABLE IF NOT EXISTS task_targets (
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			client_id TEXT NOT NULL REFERENCES clients(id),
			status TEXT NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(task_id, client_id)
		)`,
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
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='failed', error='server restarted before task completed', finished_at=? WHERE status IN ('queued','running')`, now())
	return err
}

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

func (s *Store) VerifyAdmin(ctx context.Context, password string) bool {
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='admin_password_hash'`).Scan(&hash); err != nil {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

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

func (s *Store) UpdateClientHello(ctx context.Context, id string, hello model.Hello) error {
	labels, _ := json.Marshal(hello.Labels)
	_, err := s.db.ExecContext(ctx, `UPDATE clients SET name=?,labels_json=?,version=?,os=?,arch=?,last_seen=? WHERE id=?`,
		hello.Name, string(labels), hello.Version, hello.OS, hello.Arch, now(), id)
	return err
}

func (s *Store) TouchClient(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE clients SET last_seen=? WHERE id=?`, now(), id)
	return err
}

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

func (s *Store) CreateTask(ctx context.Context, task Task, clientIDs []string) error {
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

func (s *Store) SetTargetStatus(ctx context.Context, taskID, clientID, status, detail string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE task_targets SET status=?,error=? WHERE task_id=? AND client_id=?`, status, detail, taskID, clientID)
	return err
}

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

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	var task Task
	err := s.db.QueryRowContext(ctx, `SELECT id,status,candidate_count,top_n,threads,proxy_count,client_count,error,created_at,started_at,finished_at FROM tasks WHERE id=?`, id).
		Scan(&task.ID, &task.Status, &task.CandidateCount, &task.TopN, &task.Threads, &task.ProxyCount, &task.ClientCount, &task.Error, &task.CreatedAt, &task.StartedAt, &task.FinishedAt)
	return task, err
}

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

type scanner interface {
	Scan(dest ...any) error
}

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

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
