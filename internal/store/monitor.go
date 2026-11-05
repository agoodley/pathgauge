package store

import (
	"context"
	"time"
)

// Sample is one monitoring interval for an agent.
type Sample struct {
	TS      int64   `json:"ts"` // unix seconds
	RTTMin  float64 `json:"rtt_min"`
	RTTP50  float64 `json:"rtt_p50"`
	RTTP95  float64 `json:"rtt_p95"`
	RTTMax  float64 `json:"rtt_max"`
	Jitter  float64 `json:"jitter"`
	LossPct float64 `json:"loss_pct"`
	Sent    int     `json:"sent"`
}

// AddSamples stores monitoring samples.
func (s *Store) AddSamples(ctx context.Context, agentID string, ss []Sample) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, x := range ss {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO monitor_samples(agent_id,ts,rtt_min,rtt_p50,rtt_p95,rtt_max,jitter,loss_pct,sent) VALUES(?,?,?,?,?,?,?,?,?)`,
			agentID, x.TS, x.RTTMin, x.RTTP50, x.RTTP95, x.RTTMax, x.Jitter, x.LossPct, x.Sent); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Samples returns samples between from and to, aggregated into buckets of
// step seconds (rollups are computed on query rather than stored).
func (s *Store) Samples(ctx context.Context, agentID string, from, to time.Time, step int64) ([]Sample, error) {
	if step < 1 {
		step = 1
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT (ts/?)*? AS b, MIN(rtt_min), AVG(rtt_p50), MAX(rtt_p95), MAX(rtt_max), AVG(jitter),
		CASE WHEN SUM(sent) > 0 THEN SUM(loss_pct*sent)/SUM(sent) ELSE AVG(loss_pct) END, SUM(sent)
		FROM monitor_samples WHERE agent_id=? AND ts>=? AND ts<? GROUP BY b ORDER BY b`, step, step, agentID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var x Sample
		if err := rows.Scan(&x.TS, &x.RTTMin, &x.RTTP50, &x.RTTP95, &x.RTTMax, &x.Jitter, &x.LossPct, &x.Sent); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// HeatCell is the average of a metric for one hour of one weekday.
type HeatCell struct {
	Weekday int     `json:"weekday"` // 0 = Sunday
	Hour    int     `json:"hour"`
	RTTP95  float64 `json:"rtt_p95"`
	LossPct float64 `json:"loss_pct"`
	N       int     `json:"n"`
}

// Heatmap aggregates samples by weekday and hour (UTC) since from.
func (s *Store) Heatmap(ctx context.Context, agentID string, from time.Time) ([]HeatCell, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT CAST(strftime('%w', ts, 'unixepoch') AS INTEGER), CAST(strftime('%H', ts, 'unixepoch') AS INTEGER),
		AVG(rtt_p95), AVG(loss_pct), COUNT(*) FROM monitor_samples WHERE agent_id=? AND ts>=? GROUP BY 1,2 ORDER BY 1,2`, agentID, from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeatCell
	for rows.Next() {
		var c HeatCell
		if err := rows.Scan(&c.Weekday, &c.Hour, &c.RTTP95, &c.LossPct, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LatestSample returns the newest sample for an agent.
func (s *Store) LatestSample(ctx context.Context, agentID string) (Sample, bool) {
	var x Sample
	err := s.DB.QueryRowContext(ctx, `SELECT ts,rtt_min,rtt_p50,rtt_p95,rtt_max,jitter,loss_pct,sent FROM monitor_samples WHERE agent_id=? ORDER BY ts DESC LIMIT 1`, agentID).
		Scan(&x.TS, &x.RTTMin, &x.RTTP50, &x.RTTP95, &x.RTTMax, &x.Jitter, &x.LossPct, &x.Sent)
	return x, err == nil
}

// RecentSamples returns samples newer than since.
func (s *Store) RecentSamples(ctx context.Context, agentID string, since time.Time) ([]Sample, error) {
	return s.Samples(ctx, agentID, since, time.Now().Add(time.Minute), 1)
}

// PruneSamples deletes samples older than the retention period.
func (s *Store) PruneSamples(ctx context.Context, keep time.Duration) {
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM monitor_samples WHERE ts < ?`, time.Now().Add(-keep).Unix())
}

// Schedule runs a profile on a cron schedule.
type Schedule struct {
	ID        int64  `json:"id"`
	AgentID   string `json:"agent_id"`
	Profile   string `json:"profile"`
	Tests     string `json:"tests"`
	Cron      string `json:"cron"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
	LastRun   string `json:"last_run"`
	Note      string `json:"note"`
}

// Schedules lists schedules.
func (s *Store) Schedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,agent_id,profile,tests,cron,enabled,created_at,COALESCE(last_run,''),note FROM schedules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var x Schedule
		var en int
		if err := rows.Scan(&x.ID, &x.AgentID, &x.Profile, &x.Tests, &x.Cron, &en, &x.CreatedAt, &x.LastRun, &x.Note); err != nil {
			return nil, err
		}
		x.Enabled = en != 0
		out = append(out, x)
	}
	return out, rows.Err()
}

// SaveSchedule creates (ID 0) or updates a schedule.
func (s *Store) SaveSchedule(ctx context.Context, x Schedule) (int64, error) {
	if x.ID == 0 {
		r, err := s.DB.ExecContext(ctx, `INSERT INTO schedules(agent_id,profile,tests,cron,enabled,created_at,note) VALUES(?,?,?,?,?,?,?)`,
			x.AgentID, x.Profile, x.Tests, x.Cron, boolInt(x.Enabled), Now(), x.Note)
		if err != nil {
			return 0, err
		}
		return r.LastInsertId()
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE schedules SET agent_id=?, profile=?, tests=?, cron=?, enabled=?, note=? WHERE id=?`,
		x.AgentID, x.Profile, x.Tests, x.Cron, boolInt(x.Enabled), x.Note, x.ID)
	return x.ID, err
}

// ScheduleRan records the last run time.
func (s *Store) ScheduleRan(ctx context.Context, id int64) {
	_, _ = s.DB.ExecContext(ctx, `UPDATE schedules SET last_run=? WHERE id=?`, Now(), id)
}

// DeleteSchedule removes a schedule.
func (s *Store) DeleteSchedule(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM schedules WHERE id=?`, id)
	return err
}

// AlertRule fires when a metric crosses a threshold.
type AlertRule struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	AgentID    string  `json:"agent_id"` // empty: every agent
	Metric     string  `json:"metric"`   // rtt_p95, loss_pct, jitter, offline, health_score
	Op         string  `json:"op"`       // >, <
	Threshold  float64 `json:"threshold"`
	ForMinutes int     `json:"for_minutes"`
	Channel    string  `json:"channel"` // webhook, slack, email, log
	Target     string  `json:"target"`
	Enabled    bool    `json:"enabled"`
	CreatedAt  string  `json:"created_at"`
}

// AlertRules lists rules.
func (s *Store) AlertRules(ctx context.Context) ([]AlertRule, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,agent_id,metric,op,threshold,for_minutes,channel,target,enabled,created_at FROM alert_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertRule
	for rows.Next() {
		var x AlertRule
		var en int
		if err := rows.Scan(&x.ID, &x.Name, &x.AgentID, &x.Metric, &x.Op, &x.Threshold, &x.ForMinutes, &x.Channel, &x.Target, &en, &x.CreatedAt); err != nil {
			return nil, err
		}
		x.Enabled = en != 0
		out = append(out, x)
	}
	return out, rows.Err()
}

// SaveAlertRule creates (ID 0) or updates a rule.
func (s *Store) SaveAlertRule(ctx context.Context, x AlertRule) (int64, error) {
	if x.ID == 0 {
		r, err := s.DB.ExecContext(ctx, `INSERT INTO alert_rules(name,agent_id,metric,op,threshold,for_minutes,channel,target,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			x.Name, x.AgentID, x.Metric, x.Op, x.Threshold, x.ForMinutes, x.Channel, x.Target, boolInt(x.Enabled), Now())
		if err != nil {
			return 0, err
		}
		return r.LastInsertId()
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE alert_rules SET name=?, agent_id=?, metric=?, op=?, threshold=?, for_minutes=?, channel=?, target=?, enabled=? WHERE id=?`,
		x.Name, x.AgentID, x.Metric, x.Op, x.Threshold, x.ForMinutes, x.Channel, x.Target, boolInt(x.Enabled), x.ID)
	return x.ID, err
}

// DeleteAlertRule removes a rule and its events.
func (s *Store) DeleteAlertRule(ctx context.Context, id int64) error {
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM alert_events WHERE rule_id=?`, id)
	_, err := s.DB.ExecContext(ctx, `DELETE FROM alert_rules WHERE id=?`, id)
	return err
}

// AlertEvent is one firing of a rule for one agent.
type AlertEvent struct {
	ID         int64   `json:"id"`
	RuleID     int64   `json:"rule_id"`
	RuleName   string  `json:"rule_name"`
	AgentID    string  `json:"agent_id"`
	AgentName  string  `json:"agent_name"`
	State      string  `json:"state"` // firing, resolved
	Value      float64 `json:"value"`
	Message    string  `json:"message"`
	StartedAt  string  `json:"started_at"`
	ResolvedAt string  `json:"resolved_at"`
	NotifyErr  string  `json:"notify_error"`
}

// OpenAlert returns the firing event for a rule/agent, if any.
func (s *Store) OpenAlert(ctx context.Context, ruleID int64, agentID string) (AlertEvent, bool) {
	var e AlertEvent
	err := s.DB.QueryRowContext(ctx, `SELECT id,rule_id,agent_id,state,COALESCE(value,0),message,started_at FROM alert_events WHERE rule_id=? AND agent_id=? AND state='firing'`, ruleID, agentID).
		Scan(&e.ID, &e.RuleID, &e.AgentID, &e.State, &e.Value, &e.Message, &e.StartedAt)
	return e, err == nil
}

// FireAlert opens an event.
func (s *Store) FireAlert(ctx context.Context, e AlertEvent) (int64, error) {
	r, err := s.DB.ExecContext(ctx, `INSERT INTO alert_events(rule_id,agent_id,state,value,message,started_at) VALUES(?,?,'firing',?,?,?)`, e.RuleID, e.AgentID, e.Value, e.Message, Now())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// ResolveAlert closes an event.
func (s *Store) ResolveAlert(ctx context.Context, id int64) {
	_, _ = s.DB.ExecContext(ctx, `UPDATE alert_events SET state='resolved', resolved_at=? WHERE id=?`, Now(), id)
}

// NotifyResult records a delivery error (empty for success).
func (s *Store) NotifyResult(ctx context.Context, id int64, errMsg string) {
	_, _ = s.DB.ExecContext(ctx, `UPDATE alert_events SET notified_error=? WHERE id=?`, errMsg, id)
}

// AlertEvents lists recent events.
func (s *Store) AlertEvents(ctx context.Context, limit int) ([]AlertEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT e.id,e.rule_id,COALESCE(r.name,''),e.agent_id,COALESCE(a.name,''),e.state,COALESCE(e.value,0),e.message,e.started_at,COALESCE(e.resolved_at,''),COALESCE(e.notified_error,'')
		FROM alert_events e LEFT JOIN alert_rules r ON r.id=e.rule_id LEFT JOIN agents a ON a.id=e.agent_id ORDER BY e.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertEvent
	for rows.Next() {
		var e AlertEvent
		if err := rows.Scan(&e.ID, &e.RuleID, &e.RuleName, &e.AgentID, &e.AgentName, &e.State, &e.Value, &e.Message, &e.StartedAt, &e.ResolvedAt, &e.NotifyErr); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CreateShare stores a share link.
func (s *Store) CreateShare(ctx context.Context, hash, runID, tmpl, by string, ttl time.Duration) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO shares(token_hash,run_id,template,expires_at,created_by) VALUES(?,?,?,?,?)`, hash, runID, tmpl, Ts(time.Now().Add(ttl)), by)
	return err
}

// Share resolves a share link.
func (s *Store) Share(ctx context.Context, hash string) (runID, tmpl string, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT run_id, template FROM shares WHERE token_hash=? AND expires_at > ?`, hash, Now()).Scan(&runID, &tmpl)
	if err != nil {
		return "", "", ErrNotFound
	}
	return
}

// AuditEntry is one audit record.
type AuditEntry struct {
	ID     int64  `json:"id"`
	TS     string `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

// AuditLog lists recent audit entries.
func (s *Store) AuditLog(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,ts,actor,action,target,detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Annotation marks a time range on a link's charts.
type Annotation struct {
	ID      int64  `json:"id"`
	AgentID string `json:"agent_id"`
	TSStart int64  `json:"ts_start"`
	TSEnd   int64  `json:"ts_end"`
	Text    string `json:"text"`
	Author  string `json:"author"`
}

// Annotations lists annotations for an agent.
func (s *Store) Annotations(ctx context.Context, agentID string) ([]Annotation, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,agent_id,ts_start,ts_end,text,COALESCE(author,'') FROM annotations WHERE agent_id=? ORDER BY ts_start`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Annotation
	for rows.Next() {
		var a Annotation
		if err := rows.Scan(&a.ID, &a.AgentID, &a.TSStart, &a.TSEnd, &a.Text, &a.Author); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddAnnotation stores an annotation.
func (s *Store) AddAnnotation(ctx context.Context, a Annotation) (int64, error) {
	r, err := s.DB.ExecContext(ctx, `INSERT INTO annotations(agent_id,ts_start,ts_end,text,author) VALUES(?,?,?,?,?)`, a.AgentID, a.TSStart, a.TSEnd, a.Text, a.Author)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// DeleteAnnotation removes an annotation.
func (s *Store) DeleteAnnotation(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM annotations WHERE id=?`, id)
	return err
}
