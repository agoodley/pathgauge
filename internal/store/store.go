// Package store persists server state in SQLite (WAL mode, pure-Go driver).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// Store wraps the database.
type Store struct {
	DB *sql.DB
}

// ErrNotFound is returned for missing rows.
var ErrNotFound = errors.New("not found")

// Open opens (and migrates) the database in dir.
func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, "pathgauge.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &Store{DB: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

var migrations = []string{
	`CREATE TABLE users(id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, password_hash TEXT NOT NULL, role TEXT NOT NULL,
		created_at TEXT NOT NULL, disabled INTEGER NOT NULL DEFAULT 0);
	CREATE TABLE sessions(token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, expires_at TEXT NOT NULL, created_at TEXT NOT NULL);
	CREATE TABLE api_tokens(id INTEGER PRIMARY KEY, name TEXT NOT NULL, token_hash TEXT UNIQUE NOT NULL, role TEXT NOT NULL,
		created_by TEXT, created_at TEXT NOT NULL, last_used TEXT);
	CREATE TABLE agents(id TEXT PRIMARY KEY, name TEXT NOT NULL, site TEXT NOT NULL DEFAULT '', data_key BLOB NOT NULL, cert_serial TEXT,
		version TEXT, host_json TEXT, observed_addr TEXT, last_seen TEXT, enrolled_at TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0,
		expected_down REAL NOT NULL DEFAULT 0, expected_up REAL NOT NULL DEFAULT 0, monitor_enabled INTEGER NOT NULL DEFAULT 1,
		monitor_interval INTEGER NOT NULL DEFAULT 60, policy_json TEXT, notes TEXT NOT NULL DEFAULT '');
	CREATE TABLE enroll_tokens(id INTEGER PRIMARY KEY, secret_hash TEXT UNIQUE NOT NULL, created_by TEXT, created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL, used_at TEXT, used_by TEXT, note TEXT NOT NULL DEFAULT '');
	CREATE TABLE runs(id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, profile TEXT NOT NULL, options_json TEXT NOT NULL, status TEXT NOT NULL,
		trigger TEXT NOT NULL, requested_by TEXT, created_at TEXT NOT NULL, started_at TEXT, ended_at TEXT, error TEXT,
		report_json TEXT, health_score INTEGER, down_mbps REAL, up_mbps REAL, rtt_ms REAL, bloat_grade TEXT, findings_json TEXT);
	CREATE INDEX runs_agent ON runs(agent_id, created_at);
	CREATE TABLE monitor_samples(agent_id TEXT NOT NULL, ts INTEGER NOT NULL, rtt_min REAL, rtt_p50 REAL, rtt_p95 REAL, rtt_max REAL,
		jitter REAL, loss_pct REAL, sent INTEGER, PRIMARY KEY(agent_id, ts));
	CREATE TABLE schedules(id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL, profile TEXT NOT NULL, tests TEXT NOT NULL DEFAULT '',
		cron TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, last_run TEXT, note TEXT NOT NULL DEFAULT '');
	CREATE TABLE alert_rules(id INTEGER PRIMARY KEY, name TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '', metric TEXT NOT NULL, op TEXT NOT NULL,
		threshold REAL NOT NULL, for_minutes INTEGER NOT NULL DEFAULT 5, channel TEXT NOT NULL, target TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL);
	CREATE TABLE alert_events(id INTEGER PRIMARY KEY, rule_id INTEGER NOT NULL, agent_id TEXT NOT NULL, state TEXT NOT NULL, value REAL,
		message TEXT NOT NULL, started_at TEXT NOT NULL, resolved_at TEXT, notified_error TEXT);
	CREATE TABLE shares(token_hash TEXT PRIMARY KEY, run_id TEXT NOT NULL, template TEXT NOT NULL, expires_at TEXT NOT NULL, created_by TEXT);
	CREATE TABLE audit(id INTEGER PRIMARY KEY, ts TEXT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '');
	CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
	CREATE TABLE annotations(id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL, ts_start INTEGER NOT NULL, ts_end INTEGER NOT NULL, text TEXT NOT NULL, author TEXT);`,
	// 2: an interval with no answered probe used to store its latency as 0.
	`UPDATE monitor_samples SET rtt_min=NULL, rtt_p50=NULL, rtt_p95=NULL, rtt_max=NULL, jitter=NULL WHERE loss_pct >= 100;`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version(v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(v),0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version(v) VALUES(?)`, i+1); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Now returns the canonical timestamp format.
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Ts formats a time.
func Ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// ParseTs parses a stored timestamp (zero on error).
func ParseTs(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// Audit records an action.
func (s *Store) Audit(ctx context.Context, actor, action, target, detail string) {
	_, _ = s.DB.ExecContext(ctx, `INSERT INTO audit(ts,actor,action,target,detail) VALUES(?,?,?,?,?)`, Now(), actor, action, target, detail)
}

// Setting reads a setting (empty if unset).
func (s *Store) Setting(ctx context.Context, key string) string {
	var v string
	_ = s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

// SetSetting writes a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
