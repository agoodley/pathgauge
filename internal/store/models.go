package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// User is a dashboard user.
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"` // admin, operator, viewer
	CreatedAt string `json:"created_at"`
	Disabled  bool   `json:"disabled"`
	hash      string
}

// Hash returns the stored password hash.
func (u User) Hash() string { return u.hash }

// CountUsers returns the number of users.
func (s *Store) CountUsers(ctx context.Context) int {
	var n int
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

// CreateUser adds a user.
func (s *Store) CreateUser(ctx context.Context, username, hash, role string) (int64, error) {
	r, err := s.DB.ExecContext(ctx, `INSERT INTO users(username,password_hash,role,created_at) VALUES(?,?,?,?)`, username, hash, role, Now())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var dis int
	err := row.Scan(&u.ID, &u.Username, &u.hash, &u.Role, &u.CreatedAt, &dis)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	u.Disabled = dis != 0
	return u, err
}

const userCols = `id,username,password_hash,role,created_at,disabled`

// UserByName finds a user.
func (s *Store) UserByName(ctx context.Context, name string) (User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username=?`, name))
}

// UserByID finds a user.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, id))
}

// Users lists users.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser changes role, password hash (if non-empty) or disabled.
func (s *Store) UpdateUser(ctx context.Context, id int64, role, hash string, disabled bool) error {
	if hash != "" {
		if _, err := s.DB.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, id); err != nil {
			return err
		}
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id)
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET role=?, disabled=? WHERE id=?`, role, boolInt(disabled), id)
	if disabled {
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id)
	}
	return err
}

// DeleteUser removes a user.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CreateSession stores a session token hash.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, ttl time.Duration) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at,created_at) VALUES(?,?,?,?)`, tokenHash, userID, Ts(time.Now().Add(ttl)), Now())
	return err
}

// SessionUser resolves a session.
func (s *Store) SessionUser(ctx context.Context, tokenHash string) (User, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT u.id,u.username,u.password_hash,u.role,u.created_at,u.disabled FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=? AND s.expires_at > ?`, tokenHash, Now())
	return scanUser(row)
}

// DeleteSession logs out.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) {
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
}

// APIToken is an automation credential.
type APIToken struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	LastUsed  string `json:"last_used"`
}

// CreateAPIToken stores a token hash.
func (s *Store) CreateAPIToken(ctx context.Context, name, hash, role, by string) (int64, error) {
	r, err := s.DB.ExecContext(ctx, `INSERT INTO api_tokens(name,token_hash,role,created_by,created_at) VALUES(?,?,?,?,?)`, name, hash, role, by, Now())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// APITokenRole resolves a token hash to its role and name.
func (s *Store) APITokenRole(ctx context.Context, hash string) (name, role string, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT name, role FROM api_tokens WHERE token_hash=?`, hash).Scan(&name, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err == nil {
		_, _ = s.DB.ExecContext(ctx, `UPDATE api_tokens SET last_used=? WHERE token_hash=?`, Now(), hash)
	}
	return
}

// APITokens lists tokens.
func (s *Store) APITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,role,COALESCE(created_by,''),created_at,COALESCE(last_used,'') FROM api_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Role, &t.CreatedBy, &t.CreatedAt, &t.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes a token.
func (s *Store) DeleteAPIToken(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM api_tokens WHERE id=?`, id)
	return err
}

// Agent is an enrolled agent.
type Agent struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Site            string          `json:"site"`
	DataKey         []byte          `json:"-"`
	CertSerial      string          `json:"-"`
	Version         string          `json:"version"`
	Host            json.RawMessage `json:"host,omitempty"`
	ObservedAddr    string          `json:"observed_addr"`
	LastSeen        string          `json:"last_seen"`
	EnrolledAt      string          `json:"enrolled_at"`
	Revoked         bool            `json:"revoked"`
	ExpectedDown    float64         `json:"expected_down"`
	ExpectedUp      float64         `json:"expected_up"`
	MonitorEnabled  bool            `json:"monitor_enabled"`
	MonitorInterval int             `json:"monitor_interval"`
	Policy          json.RawMessage `json:"policy,omitempty"`
	Notes           string          `json:"notes"`
	Online          bool            `json:"online"`
	Busy            bool            `json:"busy"`
}

const agentCols = `id,name,site,data_key,COALESCE(cert_serial,''),COALESCE(version,''),COALESCE(host_json,''),COALESCE(observed_addr,''),
	COALESCE(last_seen,''),enrolled_at,revoked,expected_down,expected_up,monitor_enabled,monitor_interval,COALESCE(policy_json,''),notes`

func scanAgent(row interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	var host, policy string
	var rev, mon int
	err := row.Scan(&a.ID, &a.Name, &a.Site, &a.DataKey, &a.CertSerial, &a.Version, &host, &a.ObservedAddr, &a.LastSeen, &a.EnrolledAt,
		&rev, &a.ExpectedDown, &a.ExpectedUp, &mon, &a.MonitorInterval, &policy, &a.Notes)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.Revoked, a.MonitorEnabled = rev != 0, mon != 0
	if host != "" {
		a.Host = json.RawMessage(host)
	}
	if policy != "" {
		a.Policy = json.RawMessage(policy)
	}
	return a, err
}

// CreateAgent enrolls an agent.
func (s *Store) CreateAgent(ctx context.Context, a Agent) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO agents(id,name,site,data_key,cert_serial,version,host_json,enrolled_at) VALUES(?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Site, a.DataKey, a.CertSerial, a.Version, string(a.Host), Now())
	return err
}

// Agent loads one agent.
func (s *Store) Agent(ctx context.Context, id string) (Agent, error) {
	return scanAgent(s.DB.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, id))
}

// Agents lists agents.
func (s *Store) Agents(ctx context.Context) ([]Agent, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+agentCols+` FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAgentSettings changes user-editable fields.
func (s *Store) UpdateAgentSettings(ctx context.Context, a Agent) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE agents SET name=?, site=?, expected_down=?, expected_up=?, monitor_enabled=?, monitor_interval=?, notes=? WHERE id=?`,
		a.Name, a.Site, a.ExpectedDown, a.ExpectedUp, boolInt(a.MonitorEnabled), a.MonitorInterval, a.Notes, a.ID)
	return err
}

// AgentSeen records a connection.
func (s *Store) AgentSeen(ctx context.Context, id, version, observed string, host, policy json.RawMessage) {
	if host != nil {
		_, _ = s.DB.ExecContext(ctx, `UPDATE agents SET last_seen=?, version=?, observed_addr=?, host_json=?, policy_json=? WHERE id=?`, Now(), version, observed, string(host), string(policy), id)
		return
	}
	_, _ = s.DB.ExecContext(ctx, `UPDATE agents SET last_seen=? WHERE id=?`, Now(), id)
}

// RevokeAgent disables an agent.
func (s *Store) RevokeAgent(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE agents SET revoked=1 WHERE id=?`, id)
	return err
}

// DeleteAgent removes an agent and its data.
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	for _, q := range []string{`DELETE FROM monitor_samples WHERE agent_id=?`, `DELETE FROM schedules WHERE agent_id=?`, `DELETE FROM agents WHERE id=?`} {
		if _, err := s.DB.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

// EnrollToken is a pending enrollment token.
type EnrollToken struct {
	ID        int64  `json:"id"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	UsedAt    string `json:"used_at"`
	UsedBy    string `json:"used_by"`
	Note      string `json:"note"`
}

// CreateEnrollToken stores a token secret hash.
func (s *Store) CreateEnrollToken(ctx context.Context, hash, by, note string, ttl time.Duration) (int64, error) {
	r, err := s.DB.ExecContext(ctx, `INSERT INTO enroll_tokens(secret_hash,created_by,created_at,expires_at,note) VALUES(?,?,?,?,?)`, hash, by, Now(), Ts(time.Now().Add(ttl)), note)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// UseEnrollToken consumes a valid token; it fails if expired or used.
func (s *Store) UseEnrollToken(ctx context.Context, hash, agentID string) error {
	r, err := s.DB.ExecContext(ctx, `UPDATE enroll_tokens SET used_at=?, used_by=? WHERE secret_hash=? AND used_at IS NULL AND expires_at > ?`, Now(), agentID, hash, Now())
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return errors.New("enrollment token is invalid, expired or already used")
	}
	return nil
}

// EnrollTokens lists tokens.
func (s *Store) EnrollTokens(ctx context.Context) ([]EnrollToken, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,COALESCE(created_by,''),created_at,expires_at,COALESCE(used_at,''),COALESCE(used_by,''),note FROM enroll_tokens ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnrollToken
	for rows.Next() {
		var t EnrollToken
		if err := rows.Scan(&t.ID, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.UsedAt, &t.UsedBy, &t.Note); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteEnrollToken removes a token.
func (s *Store) DeleteEnrollToken(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM enroll_tokens WHERE id=?`, id)
	return err
}

// Run is a test run.
type Run struct {
	ID          string          `json:"id"`
	AgentID     string          `json:"agent_id"`
	AgentName   string          `json:"agent_name,omitempty"`
	Profile     string          `json:"profile"`
	Options     json.RawMessage `json:"options"`
	Status      string          `json:"status"` // queued, running, done, failed, cancelled
	Trigger     string          `json:"trigger"`
	RequestedBy string          `json:"requested_by"`
	CreatedAt   string          `json:"created_at"`
	StartedAt   string          `json:"started_at"`
	EndedAt     string          `json:"ended_at"`
	Error       string          `json:"error"`
	HealthScore *int            `json:"health_score"`
	DownMbps    *float64        `json:"down_mbps"`
	UpMbps      *float64        `json:"up_mbps"`
	RTTms       *float64        `json:"rtt_ms"`
	BloatGrade  string          `json:"bloat_grade"`
	Findings    json.RawMessage `json:"findings,omitempty"`
	Report      json.RawMessage `json:"report,omitempty"`
}

const runCols = `r.id,r.agent_id,COALESCE(a.name,''),r.profile,r.options_json,r.status,r.trigger,COALESCE(r.requested_by,''),r.created_at,
	COALESCE(r.started_at,''),COALESCE(r.ended_at,''),COALESCE(r.error,''),r.health_score,r.down_mbps,r.up_mbps,r.rtt_ms,COALESCE(r.bloat_grade,''),COALESCE(r.findings_json,'')`

func scanRun(row interface{ Scan(...any) error }, withReport bool) (Run, error) {
	var r Run
	var opts, findings string
	var hs sql.NullInt64
	var down, up, rtt sql.NullFloat64
	dest := []any{&r.ID, &r.AgentID, &r.AgentName, &r.Profile, &opts, &r.Status, &r.Trigger, &r.RequestedBy, &r.CreatedAt, &r.StartedAt, &r.EndedAt, &r.Error, &hs, &down, &up, &rtt, &r.BloatGrade, &findings}
	var report sql.NullString
	if withReport {
		dest = append(dest, &report)
	}
	err := row.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	r.Options = json.RawMessage(opts)
	if findings != "" {
		r.Findings = json.RawMessage(findings)
	}
	if hs.Valid {
		v := int(hs.Int64)
		r.HealthScore = &v
	}
	if down.Valid {
		r.DownMbps = &down.Float64
	}
	if up.Valid {
		r.UpMbps = &up.Float64
	}
	if rtt.Valid {
		r.RTTms = &rtt.Float64
	}
	if report.Valid && report.String != "" {
		r.Report = json.RawMessage(report.String)
	}
	return r, err
}

// CreateRun queues a run.
func (s *Store) CreateRun(ctx context.Context, r Run) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO runs(id,agent_id,profile,options_json,status,trigger,requested_by,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		r.ID, r.AgentID, r.Profile, string(r.Options), r.Status, r.Trigger, r.RequestedBy, Now())
	return err
}

// SetRunStatus updates status (and start/end times).
func (s *Store) SetRunStatus(ctx context.Context, id, status, errMsg string) error {
	switch status {
	case "running":
		_, err := s.DB.ExecContext(ctx, `UPDATE runs SET status=?, started_at=? WHERE id=?`, status, Now(), id)
		return err
	case "done", "failed", "cancelled":
		_, err := s.DB.ExecContext(ctx, `UPDATE runs SET status=?, ended_at=?, error=? WHERE id=? AND status NOT IN ('done','failed','cancelled')`, status, Now(), errMsg, id)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE runs SET status=? WHERE id=?`, status, id)
	return err
}

// RunSummary holds denormalised headline numbers.
type RunSummary struct {
	HealthScore int
	DownMbps    float64
	UpMbps      float64
	RTTms       float64
	BloatGrade  string
	Findings    json.RawMessage
}

// FinishRun stores the report of a run and marks it done. It reports
// whether a run was updated.
//
// Only the agent the run was given to can finish it, and only while the
// run is running or failed: a result that arrives late (after a
// disconnect or a timeout) completes a failed run, but nothing replaces a
// stored report or reverses a cancellation.
func (s *Store) FinishRun(ctx context.Context, id, agentID string, report json.RawMessage, sum RunSummary) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE runs SET status='done', ended_at=?, error=NULL, report_json=?, health_score=?, down_mbps=?, up_mbps=?, rtt_ms=?, bloat_grade=?, findings_json=?
		WHERE id=? AND agent_id=? AND status IN ('running','failed')`,
		Now(), string(report), sum.HealthScore, sum.DownMbps, sum.UpMbps, sum.RTTms, sum.BloatGrade, string(sum.Findings), id, agentID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// GetRun loads a run, optionally with its report.
func (s *Store) GetRun(ctx context.Context, id string, withReport bool) (Run, error) {
	q := `SELECT ` + runCols
	if withReport {
		q += `,r.report_json`
	}
	q += ` FROM runs r LEFT JOIN agents a ON a.id=r.agent_id WHERE r.id=?`
	return scanRun(s.DB.QueryRowContext(ctx, q, id), withReport)
}

// Runs lists runs (newest first).
func (s *Store) Runs(ctx context.Context, agentID, status string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT ` + runCols + ` FROM runs r LEFT JOIN agents a ON a.id=r.agent_id WHERE 1=1`
	var args []any
	if agentID != "" {
		q += ` AND r.agent_id=?`
		args = append(args, agentID)
	}
	if status != "" {
		q += ` AND r.status IN (` + placeholders(strings.Count(status, ",")+1) + `)` //nolint:gosec // only placeholders are concatenated
		for _, st := range strings.Split(status, ",") {
			args = append(args, st)
		}
	}
	q += ` ORDER BY r.created_at DESC LIMIT ?` //nolint:gosec // only placeholders are concatenated
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// DeleteRun removes a run.
func (s *Store) DeleteRun(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM runs WHERE id=?`, id)
	return err
}

// FailStaleRuns marks runs left running by a previous server process.
func (s *Store) FailStaleRuns(ctx context.Context) {
	_, _ = s.DB.ExecContext(ctx, `UPDATE runs SET status='failed', ended_at=?, error='server restarted during the run' WHERE status IN ('queued','running')`, Now())
}
