package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	_ "modernc.org/sqlite"
)

type ModelStats struct {
	Name       string `json:"name"`
	Requests   int    `json:"requests"`
	Success    int    `json:"success"`
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheRead  int64  `json:"cacheRead"`
	CacheWrite int64  `json:"cacheWrite"`
	Reasoning  int64  `json:"reasoning"`
	Tokens     int64  `json:"tokens"`
}

type TodayStats struct {
	Requests    int          `json:"requests"`
	Success     int          `json:"success"`
	Failed      int          `json:"failed"`
	SuccessRate float64      `json:"successRate"`
	Input       int64        `json:"input"`
	Output      int64        `json:"output"`
	CacheRead   int64        `json:"cacheRead"`
	CacheWrite  int64        `json:"cacheWrite"`
	Reasoning   int64        `json:"reasoning"`
	Tokens      int64        `json:"tokens"`
	Models      []ModelStats `json:"models"`
	LastUsed    int64        `json:"lastUsed"`
}

type DB struct {
	sql *sql.DB
	mu  sync.Mutex
}

func OpenDB(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	db := &DB{sql: sqlDB}
	if _, err := sqlDB.Exec(`
CREATE TABLE IF NOT EXISTS logs (
  id TEXT PRIMARY KEY,
  time INTEGER NOT NULL,
  account_id TEXT,
  account_label TEXT,
  model TEXT,
  mode TEXT,
  status INTEGER,
  ok INTEGER,
  duration_ms INTEGER,
  first_token_ms INTEGER DEFAULT 0,
  last_token_ms INTEGER DEFAULT 0,
  stream_ms INTEGER DEFAULT 0,
  tokens_per_sec REAL DEFAULT 0,
  stream INTEGER,
  input_tokens INTEGER DEFAULT 0,
  output_tokens INTEGER DEFAULT 0,
  cache_read_tokens INTEGER DEFAULT 0,
  cache_write_tokens INTEGER DEFAULT 0,
  reasoning_tokens INTEGER DEFAULT 0,
  upstream_status INTEGER,
  error TEXT,
  request_id TEXT,
  session_id TEXT,
  client_ip TEXT,
  request_bytes INTEGER DEFAULT 0,
  response_bytes INTEGER DEFAULT 0,
  request_body TEXT,
  response_body TEXT
);
CREATE INDEX IF NOT EXISTS idx_logs_time ON logs(time DESC);
CREATE INDEX IF NOT EXISTS idx_logs_account_time ON logs(account_id, time DESC);
CREATE INDEX IF NOT EXISTS idx_logs_session ON logs(session_id);
CREATE TABLE IF NOT EXISTS session_bindings (
  session_key TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_used INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bindings_account ON session_bindings(account_id);
CREATE TABLE IF NOT EXISTS model_states (
  account_id TEXT NOT NULL,
  model TEXT NOT NULL,
  streak INTEGER NOT NULL DEFAULT 0,
  stage INTEGER NOT NULL DEFAULT 0,
  cooldown_until INTEGER NOT NULL DEFAULT 0,
  disabled INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (account_id, model)
);
CREATE INDEX IF NOT EXISTS idx_model_states_account ON model_states(account_id);
CREATE TABLE IF NOT EXISTS account_profiles (
  account_id TEXT PRIMARY KEY,
  platform TEXT NOT NULL,
  arch TEXT NOT NULL,
  os_version TEXT NOT NULL,
  language TEXT NOT NULL,
  timezone TEXT NOT NULL,
  screen TEXT NOT NULL,
  device_mid TEXT NOT NULL,
  concurrency INTEGER NOT NULL DEFAULT 1,
  last_activation_date TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS admin_sessions (
  token TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
`); err != nil {
		sqlDB.Close()
		return nil, err
	}
	if err := db.ensureLogTimingColumns(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func (d *DB) ensureLogTimingColumns() error {
	rows, err := d.sql.Query(`PRAGMA table_info(logs)`)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	columns := []struct {
		name string
		ddl  string
	}{
		{"first_token_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"last_token_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"stream_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"tokens_per_sec", "REAL NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		if existing[column.name] {
			continue
		}
		if _, err := d.sql.Exec(`ALTER TABLE logs ADD COLUMN ` + column.name + ` ` + column.ddl); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) Close() error {
	return d.sql.Close()
}

func (d *DB) InsertLog(e *LogEntry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	ok := 0
	if e.OK {
		ok = 1
	}
	stream := 0
	if e.Stream {
		stream = 1
	}
	_, err := d.sql.Exec(`
INSERT OR REPLACE INTO logs (
  id, time, account_id, account_label, model, mode, status, ok, duration_ms,
  first_token_ms, last_token_ms, stream_ms, tokens_per_sec, stream,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
  upstream_status, error, request_id, session_id, client_ip, request_bytes, response_bytes,
  request_body, response_body
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.Time, e.AccountID, e.AccountLabel, e.Model, e.Mode, e.Status, ok, e.DurationMs,
		e.FirstTokenMs, e.LastTokenMs, e.StreamMs, e.TokensPerSec, stream,
		e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.CacheReadTokens,
		e.Usage.CacheWriteTokens, e.Usage.ReasoningTokens,
		e.UpstreamStatus, e.Error, e.RequestID, e.SessionID, e.ClientIP,
		e.RequestBytes, e.ResponseBytes, e.RequestBody, e.ResponseBody)
	return err
}

func (d *DB) GetLog(id string) (*LogEntry, error) {
	row := d.sql.QueryRow(`SELECT id, time, account_id, account_label, model, mode, status, ok, duration_ms,
  first_token_ms, last_token_ms, stream_ms, tokens_per_sec, stream,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
  upstream_status, error, request_id, session_id, client_ip, request_bytes, response_bytes,
  request_body, response_body FROM logs WHERE id = ?`, id)
	return scanLog(row)
}

func (d *DB) Logs(limit int, status, accountID string) ([]*LogEntry, error) {
	query := `SELECT id, time, account_id, account_label, model, mode, status, ok, duration_ms,
  first_token_ms, last_token_ms, stream_ms, tokens_per_sec, stream,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
  upstream_status, error, request_id, session_id, client_ip, request_bytes, response_bytes,
  request_body, response_body FROM logs`
	args := []any{}
	where := []string{}
	if status == "ok" {
		where = append(where, "ok = 1")
	} else if status == "error" {
		where = append(where, "ok = 0")
	}
	if accountID != "" {
		where = append(where, "account_id = ?")
		args = append(args, accountID)
	}
	if len(where) > 0 {
		query += " WHERE " + joinAnd(where)
	}
	query += " ORDER BY time DESC LIMIT ?"
	args = append(args, limit)
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*LogEntry{}
	for rows.Next() {
		e, err := scanLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) TodayTotals(start int64) (int, int, Usage) {
	var requests, total int
	var u Usage
	_ = d.sql.QueryRow(`SELECT COUNT(*), COALESCE(SUM(input_tokens+output_tokens+cache_read_tokens+cache_write_tokens+reasoning_tokens),0),
  COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
  COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
  COALESCE(SUM(reasoning_tokens),0)
  FROM logs WHERE time >= ? AND ok = 1`, start).Scan(&requests, &total,
		&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens, &u.ReasoningTokens)
	return requests, total, u
}

func (d *DB) AccountStats(start int64) map[string]*TodayStats {
	out := map[string]*TodayStats{}
	rows, err := d.sql.Query(`SELECT account_id, model, COUNT(*), COALESCE(SUM(ok),0),
  COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
  COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
  COALESCE(SUM(reasoning_tokens),0),
  COALESCE(SUM(input_tokens+output_tokens+cache_read_tokens+cache_write_tokens+reasoning_tokens),0),
  COALESCE(MAX(time),0)
  FROM logs WHERE time >= ? GROUP BY account_id, model`, start)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var accountID, model string
		var requests, success int
		var input, output, cacheRead, cacheWrite, reasoning, tokens, lastUsed int64
		if rows.Scan(&accountID, &model, &requests, &success,
			&input, &output, &cacheRead, &cacheWrite, &reasoning, &tokens, &lastUsed) != nil {
			continue
		}
		stats := out[accountID]
		if stats == nil {
			stats = &TodayStats{}
			out[accountID] = stats
		}
		stats.Requests += requests
		stats.Success += success
		stats.Input += input
		stats.Output += output
		stats.CacheRead += cacheRead
		stats.CacheWrite += cacheWrite
		stats.Reasoning += reasoning
		stats.Tokens += tokens
		if lastUsed > stats.LastUsed {
			stats.LastUsed = lastUsed
		}
		stats.Models = append(stats.Models, ModelStats{
			Name: model, Requests: requests, Success: success,
			Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite,
			Reasoning: reasoning, Tokens: tokens,
		})
	}
	for _, stats := range out {
		stats.Failed = stats.Requests - stats.Success
		if stats.Requests > 0 {
			stats.SuccessRate = float64(stats.Success) / float64(stats.Requests) * 100
		}
	}
	return out
}

func (d *DB) GetBinding(sessionKey string, now int64) (string, bool) {
	var accountID string
	var expiresAt int64
	err := d.sql.QueryRow(`SELECT account_id, expires_at FROM session_bindings WHERE session_key = ?`, sessionKey).Scan(&accountID, &expiresAt)
	if err != nil || expiresAt <= now {
		if err == nil {
			d.Unbind(sessionKey)
		}
		return "", false
	}
	return accountID, true
}

func (d *DB) Bind(sessionKey, accountID string, now, ttl int64) {
	if sessionKey == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.sql.Exec(`INSERT INTO session_bindings(session_key, account_id, created_at, last_used, expires_at)
VALUES(?,?,?,?,?)
ON CONFLICT(session_key) DO UPDATE SET account_id=excluded.account_id, last_used=excluded.last_used, expires_at=excluded.expires_at`,
		sessionKey, accountID, now, now, now+ttl)
}

func (d *DB) TouchBinding(sessionKey string, now, ttl int64) {
	if sessionKey == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.sql.Exec(`UPDATE session_bindings SET last_used=?, expires_at=? WHERE session_key=?`, now, now+ttl, sessionKey)
}

func (d *DB) Unbind(sessionKey string) {
	if sessionKey == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.sql.Exec(`DELETE FROM session_bindings WHERE session_key=?`, sessionKey)
}

func (d *DB) PruneBindings(now int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.sql.Exec(`DELETE FROM session_bindings WHERE expires_at <= ?`, now)
}

func (d *DB) UnbindAccount(accountID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.sql.Exec(`DELETE FROM session_bindings WHERE account_id=?`, accountID)
}

func (d *DB) CreateAdminSession(token string, now, expiresAt int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`INSERT OR REPLACE INTO admin_sessions(token, created_at, expires_at) VALUES(?,?,?)`, token, now, expiresAt)
	return err
}

func (d *DB) AdminSessionValid(token string, now int64) bool {
	var expiresAt int64
	if err := d.sql.QueryRow(`SELECT expires_at FROM admin_sessions WHERE token = ?`, token).Scan(&expiresAt); err != nil {
		return false
	}
	if expiresAt <= now {
		d.DeleteAdminSession(token)
		return false
	}
	return true
}

func (d *DB) DeleteAdminSession(token string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.sql.Exec(`DELETE FROM admin_sessions WHERE token = ?`, token)
}

func (d *DB) PruneAdminSessions(now int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.sql.Exec(`DELETE FROM admin_sessions WHERE expires_at <= ?`, now)
}

func (d *DB) CountLogs() int {
	var count int
	_ = d.sql.QueryRow(`SELECT COUNT(*) FROM logs`).Scan(&count)
	return count
}

func (d *DB) ImportJSONL(path string) error {
	if d.CountLogs() > 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	count := 0
	for {
		var e LogEntry
		if err := dec.Decode(&e); err != nil {
			if err == io.EOF {
				break
			}
			break
		}
		if err := d.InsertLog(&e); err != nil {
			return err
		}
		count++
	}
	if count > 0 {
		_ = f.Close()
		_ = os.Rename(path, path+".imported")
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanLog(row rowScanner) (*LogEntry, error) {
	var e LogEntry
	var ok, stream int
	var input, output, cacheRead, cacheWrite, reasoning int
	var body, respBody sql.NullString
	err := row.Scan(&e.ID, &e.Time, &e.AccountID, &e.AccountLabel, &e.Model, &e.Mode, &e.Status, &ok,
		&e.DurationMs, &e.FirstTokenMs, &e.LastTokenMs, &e.StreamMs, &e.TokensPerSec, &stream,
		&input, &output, &cacheRead, &cacheWrite, &reasoning,
		&e.UpstreamStatus, &e.Error, &e.RequestID, &e.SessionID, &e.ClientIP,
		&e.RequestBytes, &e.ResponseBytes, &body, &respBody)
	if err != nil {
		return nil, err
	}
	e.OK = ok == 1
	e.Stream = stream == 1
	e.Usage = Usage{InputTokens: input, OutputTokens: output, CacheReadTokens: cacheRead,
		CacheWriteTokens: cacheWrite, ReasoningTokens: reasoning}
	e.RequestBody = body.String
	e.ResponseBody = respBody.String
	return &e, nil
}

func joinAnd(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += " AND "
		}
		out += part
	}
	return out
}
