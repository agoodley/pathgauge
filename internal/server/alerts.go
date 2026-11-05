package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/agoodley/pathgauge/internal/agentproto"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/store"
)

// Alert metrics.
var alertMetrics = map[string]string{
	"rtt_p95":      "monitoring RTT p95 (ms)",
	"loss_pct":     "monitoring loss (%)",
	"jitter":       "monitoring jitter (ms)",
	"offline":      "minutes since the agent was last seen",
	"health_score": "health score of the latest run",
	"down_mbps":    "download Mbit/s of the latest run",
	"up_mbps":      "upload Mbit/s of the latest run",
}

func validateRule(x store.AlertRule) error {
	if strings.TrimSpace(x.Name) == "" {
		return errors.New("name is required")
	}
	if _, ok := alertMetrics[x.Metric]; !ok {
		return fmt.Errorf("unknown metric %q", x.Metric)
	}
	if x.Op != ">" && x.Op != "<" {
		return errors.New("op must be > or <")
	}
	switch x.Channel {
	case "webhook", "slack", "ntfy":
		if !strings.HasPrefix(x.Target, "https://") && !strings.HasPrefix(x.Target, "http://") {
			return errors.New("target must be an http(s) URL")
		}
	case "email":
		if !strings.Contains(x.Target, "@") {
			return errors.New("target must be an email address")
		}
	case "log":
	default:
		return errors.New("channel must be webhook, slack, ntfy, email or log")
	}
	if x.ForMinutes < 0 || x.ForMinutes > 1440 {
		return errors.New("for_minutes must be 0-1440")
	}
	return nil
}

// Alerts evaluates rules and sends notifications.
type Alerts struct {
	s      *Server
	client *http.Client
}

func newAlerts(s *Server) *Alerts {
	return &Alerts{s: s, client: &http.Client{Timeout: 15 * time.Second}}
}

// Run evaluates monitoring rules every 30 s.
func (a *Alerts) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.evaluate(ctx)
		}
	}
}

func (a *Alerts) evaluate(ctx context.Context) {
	rules, err := a.s.store.AlertRules(ctx)
	if err != nil {
		return
	}
	agents, _ := a.s.store.Agents(ctx)
	for _, r := range rules {
		if !r.Enabled || r.Metric == "health_score" || r.Metric == "down_mbps" || r.Metric == "up_mbps" {
			continue // run-based metrics are evaluated when runs finish
		}
		for _, ag := range agents {
			if ag.Revoked || (r.AgentID != "" && r.AgentID != ag.ID) {
				continue
			}
			v, ok := a.monitorValue(ctx, r, ag)
			if !ok {
				continue
			}
			a.apply(ctx, r, ag, v)
		}
	}
}

// monitorValue averages a monitoring metric over the rule's window.
func (a *Alerts) monitorValue(ctx context.Context, r store.AlertRule, ag store.Agent) (float64, bool) {
	if r.Metric == "offline" {
		if on, _ := a.s.hub.Online(ag.ID); on {
			return 0, true
		}
		last := store.ParseTs(ag.LastSeen)
		if last.IsZero() {
			return 0, false
		}
		return time.Since(last).Minutes(), true
	}
	win := time.Duration(max(r.ForMinutes, 1)) * time.Minute
	ss, err := a.s.store.RecentSamples(ctx, ag.ID, time.Now().Add(-win))
	if err != nil || len(ss) == 0 {
		return 0, false
	}
	// Every sample in the window must breach (a sustained condition): use
	// the least-breaching value.
	best := math.NaN()
	for _, x := range ss {
		var v float64
		switch r.Metric {
		case "rtt_p95":
			v = x.RTTP95
		case "loss_pct":
			v = x.LossPct
		case "jitter":
			v = x.Jitter
		}
		if math.IsNaN(best) || (r.Op == ">" && v < best) || (r.Op == "<" && v > best) {
			best = v
		}
	}
	return best, !math.IsNaN(best)
}

func breaches(op string, v, th float64) bool {
	if op == ">" {
		return v > th
	}
	return v < th
}

func (a *Alerts) apply(ctx context.Context, r store.AlertRule, ag store.Agent, v float64) {
	open, isOpen := a.s.store.OpenAlert(ctx, r.ID, ag.ID)
	if breaches(r.Op, v, r.Threshold) {
		if isOpen {
			return
		}
		e := store.AlertEvent{RuleID: r.ID, AgentID: ag.ID, State: "firing", Value: v,
			Message: fmt.Sprintf("%s: %s on %s is %.2f (threshold %s %.2f)", r.Name, alertMetrics[r.Metric], ag.Name, v, r.Op, r.Threshold)}
		id, err := a.s.store.FireAlert(ctx, e)
		if err != nil {
			return
		}
		e.ID = id
		a.s.log.Warn("alert firing", "rule", r.Name, "agent", ag.Name, "value", v)
		a.s.hub.broadcast(map[string]any{"type": "alert", "state": "firing", "event": e})
		go a.deliver(r, e) //nolint:gosec // delivery outlives the evaluation context on purpose
		return
	}
	if isOpen {
		a.s.store.ResolveAlert(ctx, open.ID)
		open.State = "resolved"
		open.Message = fmt.Sprintf("Resolved: %s on %s is %.2f", alertMetrics[r.Metric], ag.Name, v)
		a.s.hub.broadcast(map[string]any{"type": "alert", "state": "resolved", "event": open})
		go a.deliver(r, open) //nolint:gosec // delivery outlives the evaluation context on purpose
	}
}

// onRun evaluates run-based rules when a run completes.
func (a *Alerts) onRun(ctx context.Context, agentID string, rep *results.Report) {
	rules, err := a.s.store.AlertRules(ctx)
	if err != nil {
		return
	}
	ag, err := a.s.store.Agent(ctx, agentID)
	if err != nil {
		return
	}
	for _, r := range rules {
		if !r.Enabled || (r.AgentID != "" && r.AgentID != agentID) {
			continue
		}
		var v float64
		switch r.Metric {
		case "health_score":
			v = float64(rep.Summary.HealthScore)
		case "down_mbps":
			v = rep.Summary.DownMbps
			if v == 0 {
				continue
			}
		case "up_mbps":
			v = rep.Summary.UpMbps
			if v == 0 {
				continue
			}
		default:
			continue
		}
		a.apply(ctx, r, ag, v)
	}
}

func (a *Alerts) deliver(r store.AlertRule, e store.AlertEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := a.notify(ctx, r, e)
	msg := ""
	if err != nil {
		msg = err.Error()
		a.s.log.Warn("alert notification failed", "rule", r.Name, "channel", r.Channel, "err", err)
	}
	if e.ID > 0 {
		a.s.store.NotifyResult(ctx, e.ID, msg)
	}
}

// notify sends one notification through the rule's channel.
func (a *Alerts) notify(ctx context.Context, r store.AlertRule, e store.AlertEvent) error {
	text := "PathGauge: " + e.Message
	switch r.Channel {
	case "log":
		a.s.log.Info("alert", "message", e.Message)
		return nil
	case "ntfy":
		return a.post(ctx, r.Target, "text/plain", []byte(text))
	case "slack":
		b, _ := json.Marshal(map[string]string{"text": text})
		return a.post(ctx, r.Target, "application/json", b)
	case "webhook":
		b, _ := json.Marshal(map[string]any{"text": text, "rule": r.Name, "metric": r.Metric, "agent_id": e.AgentID, "state": e.State, "value": e.Value, "message": e.Message, "time": time.Now().UTC()})
		return a.post(ctx, r.Target, "application/json", b)
	case "email":
		return a.email(ctx, r.Target, text, e.Message)
	}
	return fmt.Errorf("unknown channel %q", r.Channel)
}

func (a *Alerts) post(ctx context.Context, url, ctype string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return nil
}

func (a *Alerts) email(ctx context.Context, to, subject, body string) error {
	host := a.s.store.Setting(ctx, "smtp_host")
	if host == "" {
		return errors.New("SMTP is not configured (Settings)")
	}
	port := a.s.store.Setting(ctx, "smtp_port")
	if port == "" {
		port = "587"
	}
	from := a.s.store.Setting(ctx, "smtp_from")
	if from == "" {
		from = "pathgauge@" + host
	}
	user, pass := a.s.store.Setting(ctx, "smtp_user"), a.s.store.Setting(ctx, "smtp_password")
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", from, to, subject, body)
	addr := net.JoinHostPort(host, port)
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if user != "" {
		if err := c.Auth(smtp.PlainAuth("", user, pass, host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write([]byte(msg)); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Scheduler runs profiles on cron schedules.
type Scheduler struct {
	s      *Server
	reload chan struct{}
}

func newScheduler(s *Server) *Scheduler { return &Scheduler{s: s, reload: make(chan struct{}, 1)} }

// Reload re-reads schedules.
func (sc *Scheduler) Reload() {
	select {
	case sc.reload <- struct{}{}:
	default:
	}
}

// Run executes schedules until ctx ends.
func (sc *Scheduler) Run(ctx context.Context) {
	for {
		c := cron.New()
		list, _ := sc.s.store.Schedules(ctx)
		for _, x := range list {
			if !x.Enabled {
				continue
			}
			x := x
			if _, err := c.AddFunc(x.Cron, func() { sc.fire(ctx, x) }); err != nil {
				sc.s.log.Warn("invalid schedule", "id", x.ID, "cron", x.Cron, "err", err)
			}
		}
		c.Start()
		select {
		case <-ctx.Done():
			c.Stop()
			return
		case <-sc.reload:
			c.Stop()
		}
	}
}

func (sc *Scheduler) fire(ctx context.Context, x store.Schedule) {
	req := agentproto.RunRequest{Profile: x.Profile}
	if x.Tests != "" {
		req.Tests = strings.Split(x.Tests, ",")
	}
	sc.s.store.ScheduleRan(ctx, x.ID)
	if _, err := sc.s.hub.StartRun(ctx, x.AgentID, req, "schedule", fmt.Sprintf("schedule:%d", x.ID)); err != nil {
		sc.s.log.Warn("scheduled run skipped", "schedule", x.ID, "err", err)
		sc.s.store.Audit(ctx, fmt.Sprintf("schedule:%d", x.ID), "run.skipped", x.AgentID, err.Error())
	}
}
