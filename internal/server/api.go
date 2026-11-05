package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/robfig/cron/v3"

	"github.com/agoodley/pathgauge/internal/agentproto"
	"github.com/agoodley/pathgauge/internal/analysis"
	"github.com/agoodley/pathgauge/internal/engine"
	"github.com/agoodley/pathgauge/internal/pki"
	"github.com/agoodley/pathgauge/internal/report"
	"github.com/agoodley/pathgauge/internal/results"
	"github.com/agoodley/pathgauge/internal/store"
	"github.com/agoodley/pathgauge/internal/version"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	return dec.Decode(v)
}

func pathID(r *http.Request) int64 {
	n, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return n
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		if !strings.HasPrefix(r.URL.Path, "/api/runs/") || !strings.HasSuffix(r.URL.Path, "/report.html") {
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// Unauthenticated.
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": version.Get().Version})
	})
	mux.HandleFunc("GET /api/setup", s.apiSetupStatus)
	mux.HandleFunc("POST /api/setup", s.apiSetup)
	mux.HandleFunc("POST /api/auth/login", s.apiLogin)
	mux.HandleFunc("POST /api/auth/logout", s.apiLogout)
	mux.HandleFunc("POST /api/agent/enroll", s.apiEnroll)
	mux.HandleFunc("GET /api/agent/connect", s.hub.handleAgent)
	mux.HandleFunc("GET /ca.crt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		_, _ = w.Write(s.ca.CertPEM)
	})
	mux.HandleFunc("GET /share/{token}", s.apiShareView)

	// Viewer.
	mux.HandleFunc("GET /api/auth/me", s.require("viewer", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, principal(r.Context())) }))
	mux.HandleFunc("GET /api/overview", s.require("viewer", s.apiOverview))
	mux.HandleFunc("GET /api/agents", s.require("viewer", s.apiAgents))
	mux.HandleFunc("GET /api/agents/{aid}", s.require("viewer", s.apiAgent))
	mux.HandleFunc("GET /api/runs", s.require("viewer", s.apiRuns))
	mux.HandleFunc("GET /api/runs/{rid}", s.require("viewer", s.apiRun))
	mux.HandleFunc("GET /api/runs/{rid}/report.html", s.require("viewer", s.apiRunReport))
	mux.HandleFunc("GET /api/runs/{rid}/events", s.require("viewer", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, orEmpty(s.hub.RunEvents(r.PathValue("rid"))))
	}))
	mux.HandleFunc("GET /api/compare", s.require("viewer", s.apiCompare))
	mux.HandleFunc("GET /api/profiles", s.require("viewer", s.apiProfiles))
	mux.HandleFunc("GET /api/monitor/{aid}", s.require("viewer", s.apiMonitor))
	mux.HandleFunc("GET /api/monitor/{aid}/heatmap", s.require("viewer", s.apiHeatmap))
	mux.HandleFunc("GET /api/annotations/{aid}", s.require("viewer", s.apiAnnotations))
	mux.HandleFunc("GET /api/schedules", s.require("viewer", s.apiSchedules))
	mux.HandleFunc("GET /api/alert-rules", s.require("viewer", s.apiAlertRules))
	mux.HandleFunc("GET /api/alerts", s.require("viewer", s.apiAlertEvents))
	mux.HandleFunc("GET /api/live", s.require("viewer", s.apiLive))
	mux.HandleFunc("GET /metrics", s.require("viewer", s.apiMetrics))

	// Operator.
	mux.HandleFunc("POST /api/runs", s.require("operator", s.apiStartRun))
	mux.HandleFunc("POST /api/runs/{rid}/cancel", s.require("operator", s.apiCancelRun))
	mux.HandleFunc("POST /api/runs/{rid}/share", s.require("operator", s.apiShare))
	mux.HandleFunc("DELETE /api/runs/{rid}", s.require("operator", s.apiDeleteRun))
	mux.HandleFunc("POST /api/import", s.require("operator", s.apiImport))
	mux.HandleFunc("POST /api/annotations", s.require("operator", s.apiAddAnnotation))
	mux.HandleFunc("DELETE /api/annotations/{id}", s.require("operator", s.apiDeleteAnnotation))
	mux.HandleFunc("POST /api/schedules", s.require("operator", s.apiSaveSchedule))
	mux.HandleFunc("PUT /api/schedules/{id}", s.require("operator", s.apiSaveSchedule))
	mux.HandleFunc("DELETE /api/schedules/{id}", s.require("operator", s.apiDeleteSchedule))
	mux.HandleFunc("POST /api/alert-rules", s.require("operator", s.apiSaveAlertRule))
	mux.HandleFunc("PUT /api/alert-rules/{id}", s.require("operator", s.apiSaveAlertRule))
	mux.HandleFunc("DELETE /api/alert-rules/{id}", s.require("operator", s.apiDeleteAlertRule))
	mux.HandleFunc("POST /api/alert-rules/{id}/test", s.require("operator", s.apiTestAlertRule))
	mux.HandleFunc("PUT /api/agents/{aid}", s.require("operator", s.apiUpdateAgent))

	// Admin.
	mux.HandleFunc("DELETE /api/agents/{aid}", s.require("admin", s.apiDeleteAgent))
	mux.HandleFunc("POST /api/agents/{aid}/revoke", s.require("admin", s.apiRevokeAgent))
	mux.HandleFunc("GET /api/enroll-tokens", s.require("admin", s.apiEnrollTokens))
	mux.HandleFunc("POST /api/enroll-tokens", s.require("admin", s.apiCreateEnrollToken))
	mux.HandleFunc("DELETE /api/enroll-tokens/{id}", s.require("admin", s.apiDeleteEnrollToken))
	mux.HandleFunc("GET /api/users", s.require("admin", s.apiUsers))
	mux.HandleFunc("POST /api/users", s.require("admin", s.apiCreateUser))
	mux.HandleFunc("PUT /api/users/{id}", s.require("admin", s.apiUpdateUser))
	mux.HandleFunc("DELETE /api/users/{id}", s.require("admin", s.apiDeleteUser))
	mux.HandleFunc("GET /api/tokens", s.require("admin", s.apiTokens))
	mux.HandleFunc("POST /api/tokens", s.require("admin", s.apiCreateToken))
	mux.HandleFunc("DELETE /api/tokens/{id}", s.require("admin", s.apiDeleteToken))
	mux.HandleFunc("GET /api/audit", s.require("admin", s.apiAudit))
	mux.HandleFunc("GET /api/settings", s.require("admin", s.apiSettings))
	mux.HandleFunc("PUT /api/settings", s.require("admin", s.apiPutSettings))

	mux.Handle("/", s.uiHandler())
	return securityHeaders(mux)
}

// ---- setup and login ----

func (s *Server) apiSetupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"needs_setup": s.store.CountUsers(r.Context()) == 0, "version": version.Get().Version})
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role,omitempty"`
}

func (s *Server) apiSetup(w http.ResponseWriter, r *http.Request) {
	if s.store.CountUsers(r.Context()) > 0 {
		writeErr(w, http.StatusConflict, "setup is already complete")
		return
	}
	var c credentials
	if err := readJSON(r, &c); err != nil || strings.TrimSpace(c.Username) == "" {
		writeErr(w, 400, "username and password are required")
		return
	}
	hash, err := HashPassword(c.Password)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id, err := s.store.CreateUser(r.Context(), strings.TrimSpace(c.Username), hash, "admin")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u, _ := s.store.UserByID(r.Context(), id)
	s.store.Audit(r.Context(), u.Username, "setup.admin", u.Username, "first administrator created")
	_ = s.startSession(w, r, u)
	writeJSON(w, 200, Principal{Name: u.Username, Role: u.Role, UserID: u.ID})
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.loginAllowed(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many failed sign-ins; try again in a few minutes")
		return
	}
	var c credentials
	if err := readJSON(r, &c); err != nil {
		writeErr(w, 400, "invalid request")
		return
	}
	u, err := s.store.UserByName(r.Context(), c.Username)
	if err != nil || u.Disabled || !CheckPassword(u.Hash(), c.Password) {
		s.loginFailed(ip)
		s.store.Audit(r.Context(), c.Username, "login.failed", ip, "")
		writeErr(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	if err := s.startSession(w, r, u); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.store.Audit(r.Context(), u.Username, "login", ip, "")
	writeJSON(w, 200, Principal{Name: u.Username, Role: u.Role, UserID: u.ID})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.store.DeleteSession(r.Context(), pki.HashSecret(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- enrollment ----

func (s *Server) apiEnroll(w http.ResponseWriter, r *http.Request) {
	var req agentproto.EnrollRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid request")
		return
	}
	caPrefix, secret, err := pki.ParseJoinToken(req.Token)
	if err != nil || !strings.HasPrefix(s.ca.Fingerprint(), caPrefix) {
		writeErr(w, http.StatusUnauthorized, "invalid join token")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 64 {
		writeErr(w, 400, "agent name must be 1-64 characters")
		return
	}
	id := "agt_" + engine.NewRunID()
	if err := s.store.UseEnrollToken(r.Context(), pki.HashSecret(secret), id); err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	certPEM, serial, err := s.ca.SignCSR([]byte(req.CSR), id)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	if err := s.store.CreateAgent(r.Context(), store.Agent{ID: id, Name: name, DataKey: key, CertSerial: serial, Version: req.Version, Host: req.Host}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.store.Audit(r.Context(), "agent:"+name, "agent.enroll", id, clientIP(r))
	s.log.Info("agent enrolled", "agent", name, "id", id, "remote", clientIP(r))
	writeJSON(w, 200, agentproto.EnrollResponse{AgentID: id, Cert: string(certPEM), CA: string(s.ca.CertPEM), DataKey: key})
}

// CreateJoinToken issues an enrollment token (used by the API and CLI).
func (s *Server) CreateJoinToken(ctx context.Context, by, note string, ttl time.Duration) (string, error) {
	tok, secret := s.ca.NewJoinToken()
	if _, err := s.store.CreateEnrollToken(ctx, pki.HashSecret(secret), by, note, ttl); err != nil {
		return "", err
	}
	s.store.Audit(ctx, by, "enroll_token.create", note, ttl.String())
	return tok, nil
}

func (s *Server) apiEnrollTokens(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.EnrollTokens(r.Context())
	respond(w, orEmpty(t), err)
}

func (s *Server) apiCreateEnrollToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hours float64 `json:"hours"`
		Note  string  `json:"note"`
	}
	_ = readJSON(r, &req)
	if req.Hours <= 0 || req.Hours > 24*30 {
		req.Hours = 24
	}
	tok, err := s.CreateJoinToken(r.Context(), principal(r.Context()).Name, req.Note, time.Duration(req.Hours*float64(time.Hour)))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok, "expires_in_hours": req.Hours,
		"join_command": fmt.Sprintf("sudo pathgauge agent join https://%s --token %s --name <agent-name>", r.Host, tok)})
}

func (s *Server) apiDeleteEnrollToken(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]bool{"ok": true}, s.store.DeleteEnrollToken(r.Context(), pathID(r)))
}

func respond(w http.ResponseWriter, v any, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, "not found")
	case err != nil:
		writeErr(w, 400, err.Error())
	default:
		writeJSON(w, 200, v)
	}
}

// ---- agents ----

func (s *Server) decorate(a *store.Agent) {
	a.Online, _ = s.hub.Online(a.ID)
	_, busy := s.hub.Online(a.ID)
	a.Busy = busy != ""
}

func (s *Server) apiAgents(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.Agents(r.Context())
	for i := range list {
		s.decorate(&list[i])
		list[i].Host = nil // keep the list light
	}
	if list == nil {
		list = []store.Agent{}
	}
	respond(w, list, err)
}

func (s *Server) apiAgent(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.Agent(r.Context(), r.PathValue("aid"))
	if err == nil {
		s.decorate(&a)
	}
	respond(w, a, err)
}

func (s *Server) apiUpdateAgent(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.Agent(r.Context(), r.PathValue("aid"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	// Partial update: only fields present in the request change.
	var in struct {
		Name            *string  `json:"name"`
		Site            *string  `json:"site"`
		ExpectedDown    *float64 `json:"expected_down"`
		ExpectedUp      *float64 `json:"expected_up"`
		MonitorEnabled  *bool    `json:"monitor_enabled"`
		MonitorInterval *int     `json:"monitor_interval"`
		Notes           *string  `json:"notes"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "invalid request")
		return
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		a.Name = strings.TrimSpace(*in.Name)
	}
	if in.Site != nil {
		a.Site = *in.Site
	}
	if in.ExpectedDown != nil {
		a.ExpectedDown = *in.ExpectedDown
	}
	if in.ExpectedUp != nil {
		a.ExpectedUp = *in.ExpectedUp
	}
	if in.MonitorEnabled != nil {
		a.MonitorEnabled = *in.MonitorEnabled
	}
	if in.MonitorInterval != nil && *in.MonitorInterval >= 10 {
		a.MonitorInterval = *in.MonitorInterval
	}
	if in.Notes != nil {
		a.Notes = *in.Notes
	}
	if err := s.store.UpdateAgentSettings(r.Context(), a); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.hub.sendConfig(a)
	s.store.Audit(r.Context(), principal(r.Context()).Name, "agent.update", a.ID, a.Name)
	s.decorate(&a)
	writeJSON(w, 200, a)
}

func (s *Server) apiRevokeAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("aid")
	err := s.store.RevokeAgent(r.Context(), id)
	s.forgetKey(id)
	s.hub.disconnect(id)
	s.store.Audit(r.Context(), principal(r.Context()).Name, "agent.revoke", id, "")
	respond(w, map[string]bool{"ok": true}, err)
}

func (s *Server) apiDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("aid")
	_ = s.store.RevokeAgent(r.Context(), id)
	s.forgetKey(id)
	s.hub.disconnect(id)
	err := s.store.DeleteAgent(r.Context(), id)
	s.store.Audit(r.Context(), principal(r.Context()).Name, "agent.delete", id, "")
	respond(w, map[string]bool{"ok": true}, err)
}

// ---- runs ----

func (s *Server) apiRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.store.Runs(r.Context(), r.URL.Query().Get("agent"), r.URL.Query().Get("status"), limit)
	if runs == nil {
		runs = []store.Run{}
	}
	respond(w, runs, err)
}

func (s *Server) apiRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.Context(), r.PathValue("rid"), true)
	respond(w, run, err)
}

func (s *Server) apiStartRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agent_id"`
		agentproto.RunRequest
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid request")
		return
	}
	id, err := s.hub.StartRun(r.Context(), req.AgentID, req.RunRequest, "manual", principal(r.Context()).Name)
	switch {
	case errors.Is(err, ErrAgentBusy):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeErr(w, 400, err.Error())
	default:
		writeJSON(w, 200, map[string]string{"run_id": id})
	}
}

func (s *Server) apiCancelRun(w http.ResponseWriter, r *http.Request) {
	err := s.hub.CancelRun(r.PathValue("rid"))
	if err == nil {
		s.store.Audit(r.Context(), principal(r.Context()).Name, "run.cancel", r.PathValue("rid"), "")
	}
	respond(w, map[string]bool{"ok": true}, err)
}

func (s *Server) apiDeleteRun(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteRun(r.Context(), r.PathValue("rid"))
	s.store.Audit(r.Context(), principal(r.Context()).Name, "run.delete", r.PathValue("rid"), "")
	respond(w, map[string]bool{"ok": true}, err)
}

func (s *Server) loadReport(ctx context.Context, id string) (*results.Report, store.Run, error) {
	run, err := s.store.GetRun(ctx, id, true)
	if err != nil {
		return nil, run, err
	}
	if len(run.Report) == 0 {
		return nil, run, errors.New("this run has no results yet")
	}
	var rep results.Report
	if err := json.Unmarshal(run.Report, &rep); err != nil {
		return nil, run, err
	}
	return &rep, run, nil
}

func (s *Server) renderReport(w http.ResponseWriter, r *http.Request, id, tmpl string, download bool) {
	rep, _, err := s.loadReport(r.Context(), id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	var buf bytes.Buffer
	if err := report.WriteHTML(&buf, rep, report.Options{Template: tmpl, Org: s.store.Setting(r.Context(), "org_name")}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:")
	if download {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="pathgauge-%s-%s.html"`, rep.StartedAt.Format("20060102-1504"), id[:6]))
	}
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) apiRunReport(w http.ResponseWriter, r *http.Request) {
	s.renderReport(w, r, r.PathValue("rid"), r.URL.Query().Get("template"), r.URL.Query().Get("download") != "")
}

func (s *Server) apiShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hours    float64 `json:"hours"`
		Template string  `json:"template"`
	}
	_ = readJSON(r, &req)
	if req.Hours <= 0 || req.Hours > 24*90 {
		req.Hours = 72
	}
	if _, err := s.store.GetRun(r.Context(), r.PathValue("rid"), false); err != nil {
		respond(w, nil, err)
		return
	}
	tok := randToken("")
	if err := s.store.CreateShare(r.Context(), pki.HashSecret(tok), r.PathValue("rid"), req.Template, principal(r.Context()).Name, time.Duration(req.Hours*float64(time.Hour))); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.store.Audit(r.Context(), principal(r.Context()).Name, "run.share", r.PathValue("rid"), fmt.Sprintf("%.0fh", req.Hours))
	writeJSON(w, 200, map[string]string{"url": "https://" + r.Host + "/share/" + tok})
}

func (s *Server) apiShareView(w http.ResponseWriter, r *http.Request) {
	runID, tmpl, err := s.store.Share(r.Context(), pki.HashSecret(r.PathValue("token")))
	if err != nil {
		http.Error(w, "This link is invalid or has expired.", http.StatusNotFound)
		return
	}
	s.renderReport(w, r, runID, tmpl, false)
}

func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	var rep results.Report
	if err := readJSON(r, &rep); err != nil || rep.Schema == 0 {
		writeErr(w, 400, "not a PathGauge results file")
		return
	}
	analysis.Analyze(&rep, analysis.Expectations{})
	id := engine.NewRunID()
	rep.ID = id
	b, _ := json.Marshal(&rep)
	fj, _ := json.Marshal(rep.Findings)
	agentID := r.URL.Query().Get("agent")
	if agentID == "" {
		agentID = "imported"
	}
	opts, _ := json.Marshal(map[string]any{"imported": true, "client": rep.Client.Name, "endpoint": rep.Endpoint.Name})
	if err := s.store.CreateRun(r.Context(), store.Run{ID: id, AgentID: agentID, Profile: rep.Profile, Options: opts, Status: "running", Trigger: "import", RequestedBy: principal(r.Context()).Name}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	err := s.store.FinishRun(r.Context(), id, b, store.RunSummary{HealthScore: rep.Summary.HealthScore, DownMbps: rep.Summary.DownMbps, UpMbps: rep.Summary.UpMbps,
		RTTms: rep.Summary.IdleRTTms, BloatGrade: rep.Summary.BloatGrade, Findings: fj})
	s.store.Audit(r.Context(), principal(r.Context()).Name, "run.import", id, "")
	respond(w, map[string]string{"run_id": id}, err)
}

// apiCompare returns two runs' summaries side by side.
func (s *Server) apiCompare(w http.ResponseWriter, r *http.Request) {
	ids := strings.Split(r.URL.Query().Get("runs"), ",")
	if len(ids) < 2 {
		writeErr(w, 400, "pass ?runs=ID1,ID2")
		return
	}
	type item struct {
		ID       string            `json:"id"`
		Agent    string            `json:"agent"`
		Started  time.Time         `json:"started_at"`
		Profile  string            `json:"profile"`
		Summary  results.Summary   `json:"summary"`
		Findings []results.Finding `json:"findings"`
	}
	var out []item
	for _, id := range ids[:min(len(ids), 4)] {
		rep, run, err := s.loadReport(r.Context(), strings.TrimSpace(id))
		if err != nil {
			writeErr(w, 404, id+": "+err.Error())
			return
		}
		out = append(out, item{ID: run.ID, Agent: run.AgentName, Started: rep.StartedAt, Profile: rep.Profile, Summary: rep.Summary, Findings: rep.Findings})
	}
	writeJSON(w, 200, out)
}

func (s *Server) apiProfiles(w http.ResponseWriter, r *http.Request) {
	type mixKind struct {
		Name  string `json:"name"`
		Label string `json:"label"`
		Class string `json:"class"`
		Max   int    `json:"max"`
	}
	var kinds []mixKind
	for _, k := range engine.MixKinds {
		kinds = append(kinds, mixKind{k.Name, k.Label, k.Class, k.Max})
	}
	writeJSON(w, 200, map[string]any{"profiles": engine.Profiles, "tests": engine.AllTests, "titles": engine.Titles,
		"mix_kinds": kinds, "mix_scenarios": engine.MixScenarios, "mix_default": engine.DefaultMix})
}

// ---- overview ----

func (s *Server) apiOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agents, _ := s.store.Agents(ctx)
	type link struct {
		Agent    store.Agent   `json:"agent"`
		LastRun  *store.Run    `json:"last_run,omitempty"`
		Latest   *store.Sample `json:"latest_sample,omitempty"`
		Spark    []float64     `json:"spark_rtt_p95"`
		Alerting int           `json:"alerts_firing"`
	}
	events, _ := s.store.AlertEvents(ctx, 200)
	var links []link
	for _, a := range agents {
		if a.Revoked {
			continue
		}
		s.decorate(&a)
		a.Host = nil
		l := link{Agent: a, Spark: []float64{}}
		if runs, _ := s.store.Runs(ctx, a.ID, "done", 1); len(runs) > 0 {
			l.LastRun = &runs[0]
		}
		if x, ok := s.store.LatestSample(ctx, a.ID); ok {
			l.Latest = &x
		}
		if ss, _ := s.store.Samples(ctx, a.ID, time.Now().Add(-24*time.Hour), time.Now(), 1800); len(ss) > 0 {
			for _, x := range ss {
				l.Spark = append(l.Spark, x.RTTP95)
			}
		}
		for _, e := range events {
			if e.AgentID == a.ID && e.State == "firing" {
				l.Alerting++
			}
		}
		links = append(links, l)
	}
	running, _ := s.store.Runs(ctx, "", "running", 20)
	recent, _ := s.store.Runs(ctx, "", "", 10)
	var firing []store.AlertEvent
	for _, e := range events {
		if e.State == "firing" {
			firing = append(firing, e)
		}
	}
	writeJSON(w, 200, map[string]any{"links": orEmpty(links), "running": orEmpty(running), "recent": orEmpty(recent), "alerts": orEmpty(firing),
		"server_name": s.serverName(ctx), "ca_fingerprint": s.ca.Fingerprint()})
}

func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// ---- monitoring ----

func (s *Server) apiMonitor(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hours, _ := strconv.ParseFloat(q.Get("hours"), 64)
	if hours <= 0 {
		hours = 24
	}
	to := time.Now()
	from := to.Add(-time.Duration(hours * float64(time.Hour)))
	step := int64(60)
	switch {
	case hours > 24*14:
		step = 3600
	case hours > 48:
		step = 900
	case hours > 6:
		step = 300
	}
	if v, err := strconv.ParseInt(q.Get("step"), 10, 64); err == nil && v > 0 {
		step = v
	}
	ss, err := s.store.Samples(r.Context(), r.PathValue("aid"), from, to, step)
	respond(w, map[string]any{"step": step, "samples": orEmpty(ss)}, err)
}

func (s *Server) apiHeatmap(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 28
	}
	cells, err := s.store.Heatmap(r.Context(), r.PathValue("aid"), time.Now().AddDate(0, 0, -days))
	respond(w, orEmpty(cells), err)
}

func (s *Server) apiAnnotations(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.Annotations(r.Context(), r.PathValue("aid"))
	respond(w, orEmpty(a), err)
}

func (s *Server) apiAddAnnotation(w http.ResponseWriter, r *http.Request) {
	var a store.Annotation
	if err := readJSON(r, &a); err != nil || a.AgentID == "" || a.Text == "" {
		writeErr(w, 400, "agent_id and text are required")
		return
	}
	if a.TSEnd < a.TSStart {
		a.TSEnd = a.TSStart
	}
	a.Author = principal(r.Context()).Name
	id, err := s.store.AddAnnotation(r.Context(), a)
	a.ID = id
	respond(w, a, err)
}

func (s *Server) apiDeleteAnnotation(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]bool{"ok": true}, s.store.DeleteAnnotation(r.Context(), pathID(r)))
}

// ---- schedules ----

func (s *Server) apiSchedules(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.Schedules(r.Context())
	respond(w, orEmpty(list), err)
}

func (s *Server) apiSaveSchedule(w http.ResponseWriter, r *http.Request) {
	var x store.Schedule
	if err := readJSON(r, &x); err != nil {
		writeErr(w, 400, "invalid request")
		return
	}
	x.ID = pathID(r)
	if _, err := cron.ParseStandard(x.Cron); err != nil {
		writeErr(w, 400, "invalid cron expression: "+err.Error())
		return
	}
	var tests []string
	if x.Tests != "" {
		tests = strings.Split(x.Tests, ",")
	}
	if _, err := (engine.Options{Profile: x.Profile, Tests: tests}).TestList(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if _, err := s.store.Agent(r.Context(), x.AgentID); err != nil {
		writeErr(w, 400, "unknown agent")
		return
	}
	id, err := s.store.SaveSchedule(r.Context(), x)
	x.ID = id
	s.sched.Reload()
	s.store.Audit(r.Context(), principal(r.Context()).Name, "schedule.save", strconv.FormatInt(id, 10), x.Cron)
	respond(w, x, err)
}

func (s *Server) apiDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteSchedule(r.Context(), pathID(r))
	s.sched.Reload()
	respond(w, map[string]bool{"ok": true}, err)
}

// ---- alerts ----

func (s *Server) apiAlertRules(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.AlertRules(r.Context())
	respond(w, orEmpty(list), err)
}

func (s *Server) apiAlertEvents(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.AlertEvents(r.Context(), 200)
	respond(w, orEmpty(list), err)
}

func (s *Server) apiSaveAlertRule(w http.ResponseWriter, r *http.Request) {
	var x store.AlertRule
	if err := readJSON(r, &x); err != nil {
		writeErr(w, 400, "invalid request")
		return
	}
	x.ID = pathID(r)
	if err := validateRule(x); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id, err := s.store.SaveAlertRule(r.Context(), x)
	x.ID = id
	s.store.Audit(r.Context(), principal(r.Context()).Name, "alert_rule.save", strconv.FormatInt(id, 10), x.Name)
	respond(w, x, err)
}

func (s *Server) apiDeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]bool{"ok": true}, s.store.DeleteAlertRule(r.Context(), pathID(r)))
}

func (s *Server) apiTestAlertRule(w http.ResponseWriter, r *http.Request) {
	rules, _ := s.store.AlertRules(r.Context())
	for _, x := range rules {
		if x.ID == pathID(r) {
			err := s.alerts.notify(r.Context(), x, store.AlertEvent{AgentID: "test", Message: "Test notification from PathGauge for rule " + x.Name, State: "firing"})
			respond(w, map[string]bool{"ok": true}, err)
			return
		}
	}
	writeErr(w, 404, "not found")
}

// ---- users, tokens, audit, settings ----

func (s *Server) apiUsers(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.Users(r.Context())
	respond(w, orEmpty(u), err)
}

func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := readJSON(r, &c); err != nil || c.Username == "" || !ValidRole(c.Role) {
		writeErr(w, 400, "username, password and a role (admin, operator, viewer) are required")
		return
	}
	hash, err := HashPassword(c.Password)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id, err := s.store.CreateUser(r.Context(), c.Username, hash, c.Role)
	s.store.Audit(r.Context(), principal(r.Context()).Name, "user.create", c.Username, c.Role)
	respond(w, map[string]int64{"id": id}, err)
}

func (s *Server) apiUpdateUser(w http.ResponseWriter, r *http.Request) {
	// Partial update: only fields present in the request change.
	var in struct {
		Role     *string `json:"role"`
		Password *string `json:"password"`
		Disabled *bool   `json:"disabled"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "invalid request")
		return
	}
	id := pathID(r)
	u, err := s.store.UserByID(r.Context(), id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	role, disabled, hash := u.Role, u.Disabled, ""
	if in.Role != nil {
		if !ValidRole(*in.Role) {
			writeErr(w, 400, "role must be admin, operator or viewer")
			return
		}
		role = *in.Role
	}
	if in.Disabled != nil {
		disabled = *in.Disabled
	}
	if in.Password != nil && *in.Password != "" {
		if hash, err = HashPassword(*in.Password); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if id == principal(r.Context()).UserID && (role != "admin" || disabled) {
		writeErr(w, 400, "you cannot demote or disable yourself")
		return
	}
	err = s.store.UpdateUser(r.Context(), id, role, hash, disabled)
	s.store.Audit(r.Context(), principal(r.Context()).Name, "user.update", strconv.FormatInt(id, 10), role)
	respond(w, map[string]bool{"ok": true}, err)
}

func (s *Server) apiDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if id == principal(r.Context()).UserID {
		writeErr(w, 400, "you cannot delete yourself")
		return
	}
	err := s.store.DeleteUser(r.Context(), id)
	s.store.Audit(r.Context(), principal(r.Context()).Name, "user.delete", strconv.FormatInt(id, 10), "")
	respond(w, map[string]bool{"ok": true}, err)
}

func (s *Server) apiTokens(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.APITokens(r.Context())
	respond(w, orEmpty(t), err)
}

func (s *Server) apiCreateToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := readJSON(r, &in); err != nil || in.Name == "" || !ValidRole(in.Role) {
		writeErr(w, 400, "name and role are required")
		return
	}
	tok := randToken("pgt_")
	id, err := s.store.CreateAPIToken(r.Context(), in.Name, pki.HashSecret(tok), in.Role, principal(r.Context()).Name)
	s.store.Audit(r.Context(), principal(r.Context()).Name, "token.create", in.Name, in.Role)
	respond(w, map[string]any{"id": id, "token": tok}, err)
}

func (s *Server) apiDeleteToken(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteAPIToken(r.Context(), pathID(r))
	s.store.Audit(r.Context(), principal(r.Context()).Name, "token.delete", r.PathValue("id"), "")
	respond(w, map[string]bool{"ok": true}, err)
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.AuditLog(r.Context(), 500)
	respond(w, orEmpty(a), err)
}

var settingKeys = []string{"server_name", "org_name", "smtp_host", "smtp_port", "smtp_user", "smtp_password", "smtp_from"}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{}
	for _, k := range settingKeys {
		v := s.store.Setting(r.Context(), k)
		if k == "smtp_password" && v != "" {
			v = "********"
		}
		out[k] = v
	}
	writeJSON(w, 200, out)
}

func (s *Server) apiPutSettings(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "invalid request")
		return
	}
	for _, k := range settingKeys {
		v, ok := in[k]
		if !ok || (k == "smtp_password" && v == "********") {
			continue
		}
		if err := s.store.SetSetting(r.Context(), k, v); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	s.store.Audit(r.Context(), principal(r.Context()).Name, "settings.update", "", "")
	s.apiSettings(w, r)
}

// ---- live updates ----

func (s *Server) apiLive(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ch, unsub := s.hub.subscribe()
	defer unsub()
	ctx := ws.CloseRead(r.Context())
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-ch:
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := ws.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				return
			}
		case <-ping.C:
			if err := ws.Ping(ctx); err != nil {
				return
			}
		}
	}
}
