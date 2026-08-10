package assistant_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aiops/internal/agent"
	"aiops/internal/alert"
	"aiops/internal/assistant"
	"aiops/internal/eventbus"
	"aiops/internal/incident"
	"aiops/internal/llm"
	"aiops/internal/orchestrator"
	"aiops/internal/pusher"
	"aiops/internal/util"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// actions_it_test.go：人工确认动作（Phase 3）DB 门控集成测试。
// 覆盖：动作工具注册门控、提出后不执行、确认执行/拒绝/重复/过期/未装配、
// ack→状态变化、resolve、approve（编排器 pending_approval 闭环）、审计回溯。

const actTenant = "asst-act-it"

func openActDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openDB(t)
	models := []any{
		&assistant.AssistantActionRequest{}, &assistant.AssistantRun{}, &assistant.AssistantMessage{},
		&assistant.AssistantThread{}, &assistant.AssistantEvent{}, &assistant.AssistantStep{},
		&incident.Incident{}, &incident.State{}, &incident.AlertToIncident{},
		&alert.Alert{}, &eventbus.AuditLog{},
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cleanupAct(t, db)
	return db
}

func cleanupAct(t *testing.T, db *gorm.DB) {
	t.Helper()
	t.Cleanup(func() {
		for _, tbl := range []string{"assistant_action_requests", "assistant_runs", "assistant_messages",
			"assistant_threads", "assistant_events", "assistant_steps", "incident_states",
			"alert_to_incidents", "incidents", "alerts", "audit_logs"} {
			_ = db.Exec("DELETE FROM "+tbl+" WHERE tenant_id = ?", actTenant).Error
		}
	})
}

func seedActAlert(t *testing.T, db *gorm.DB, id, status string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO alerts (id, tenant_id, provider, provider_type, fingerprint, alert_hash, event,
		name, severity, status, labels, started_at, last_seen_at, created_at, updated_at)
		VALUES (?, ?, 'webhook', 'generic', ?, 'h', '{}'::jsonb, 'pay-down', 'critical', ?, '{}'::jsonb,
			now(), now(), now(), now())`,
		id, actTenant, "fp-"+id, status).Error; err != nil {
		t.Fatal(err)
	}
}

func seedActIncident(t *testing.T, db *gorm.DB, id, status string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO incidents (id, tenant_id, name, summary, severity, status, source, created_at)
		VALUES (?, ?, 'pay 服务不可用', 'pay api timeout', 'critical', ?, 'agent', now())`,
		id, actTenant, status).Error; err != nil {
		t.Fatal(err)
	}
}

// seedPendingApprovalState 构造 pending_approval 编排器状态（heal 节点带待审批动作）。
func seedPendingApprovalState(t *testing.T, db *gorm.DB, incidentID string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO incident_states (id, tenant_id, status, alert_id, correlation_id,
		node_results, current_agent, retry_count, created_at, updated_at)
		VALUES (?, ?, 'pending_approval', 'alert-pa', 'corr-1', ?::jsonb, 'heal', 0, now(), now())`,
		incidentID, actTenant, `{"monitor":{"severity":"critical"},"rca":{"rootCause":"db 连接池耗尽"},
			"heal":{"status":"awaiting_approval","action":"扩容 db 副本","level":"L1","blastRadius":0.35},
			"change":{"action":"scale_up","riskScore":0.4,"level":"L1"}}`).Error; err != nil {
		t.Fatal(err)
	}
}

// newActService 组装带执行器的 Service。
func newActService(t *testing.T, db *gorm.DB, withExecutor bool) *assistant.Service {
	t.Helper()
	incSvc := incident.NewService(db)
	svc := assistant.NewService(db, "mock", nil, eventbus.NewAuditService(db), nil, incSvc)
	if withExecutor {
		svc.SetExecutor(&assistant.ActionExecutor{
			Alerts: alert.NewRepository(db), Incidents: incSvc,
			Orchestrator: orchestrator.New(alert.NewRepository(db), incSvc, nil,
				agent.Cfg{}, eventbus.NewMemoryBus(), noopBroadcaster{}, eventbus.NewAuditService(db), nil),
		})
	}
	return svc
}

// noopBroadcaster 编排器 push 广播 no-op（审批闭环会广播 incident 状态）。
type noopBroadcaster struct{}

func (noopBroadcaster) Broadcast(ev pusher.Event) {}

// seedPendingAction 直接插入一条 pending 动作请求。
func seedPendingAction(t *testing.T, db *gorm.DB, toolName string, args map[string]any, risk string) *assistant.AssistantActionRequest {
	t.Helper()
	raw, _ := json.Marshal(args)
	req := &assistant.AssistantActionRequest{
		ID: util.NewID(), TenantID: actTenant, RunID: "run-act-1",
		ToolName: toolName, Arguments: datatypes.JSON(raw), RiskLevel: risk,
		Status: assistant.ActionPending, RequestedBy: "local-noauth",
		IdempotencyKey: "asst:" + util.NewID(),
		ExpiresAt:      timePtr(time.Now().UTC().Add(15 * time.Minute)),
		CreatedAt:      time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := assistant.CreateActionRequest(context.Background(), db, req); err != nil {
		t.Fatal(err)
	}
	return req
}

// timePtr 时间指针（外部测试包副本）。
func timePtr(t time.Time) *time.Time { return &t }

func TestActionToolsRegisteredGated(t *testing.T) {
	// 无依赖 → 不注册
	regNil := assistant.NewRegistry(assistant.ToolDeps{})
	for _, name := range []string{"ack_alert", "resolve_incident", "approve_incident_action"} {
		if _, ok := regNil.Get(name); ok {
			t.Fatalf("%s 不应在无依赖时注册", name)
		}
	}
	// 有依赖 → 注册且为动作工具（Action 非 nil、风险等级正确）
	reg := assistant.NewRegistry(assistant.ToolDeps{
		Alerts: alert.NewRepository(nil), Incidents: incident.NewService(nil),
	})
	expect := map[string]string{"ack_alert": assistant.RiskLow, "resolve_incident": assistant.RiskMedium,
		"approve_incident_action": assistant.RiskHigh}
	for name, risk := range expect {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("%s 应注册", name)
		}
		if tool.ReadOnly || tool.Action == nil || tool.Action.RiskLevel != risk {
			t.Fatalf("%s 应为动作工具（readonly=%v risk=%s）", name, tool.ReadOnly, risk)
		}
	}
}

func TestActionAckConfirmRejectFlow(t *testing.T) {
	db := openActDB(t)
	ctx := context.Background()
	seedActAlert(t, db, "alert-ack", alert.StatusFiring)

	svc := newActService(t, db, true)

	// 确认 → alert acknowledged + 请求 succeeded
	req := seedPendingAction(t, db, "ack_alert", map[string]any{"alert_id": "alert-ack"}, assistant.RiskLow)
	got, err := svc.ConfirmAction(ctx, actTenant, req.ID, "local-noauth", "确认处理")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != assistant.ActionSucceeded {
		t.Fatalf("status = %s, want succeeded", got.Status)
	}
	var st string
	if err := db.Raw(`SELECT status FROM alerts WHERE id='alert-ack' AND tenant_id=?`, actTenant).Scan(&st).Error; err != nil {
		t.Fatal(err)
	}
	if st != alert.StatusAcknowledged {
		t.Fatalf("alert status = %s, want acknowledged", st)
	}
	var n int64
	db.Raw(`SELECT count(*) FROM audit_logs WHERE tenant_id=? AND action='assistant.action.executed' AND target=?`,
		actTenant, req.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("审计应记录 executed: %d", n)
	}

	// 重复确认 → 已处理
	if _, err := svc.ConfirmAction(ctx, actTenant, req.ID, "local-noauth", ""); err == nil {
		t.Fatal("重复确认应报错")
	}

	// 拒绝 → rejected + 目标状态不变
	seedActAlert(t, db, "alert-rj", alert.StatusFiring)
	req2 := seedPendingAction(t, db, "ack_alert", map[string]any{"alert_id": "alert-rj"}, assistant.RiskLow)
	got2, err := svc.RejectAction(ctx, actTenant, req2.ID, "local-noauth", "不处理")
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != assistant.ActionRejected {
		t.Fatalf("status = %s, want rejected", got2.Status)
	}
	db.Raw(`SELECT status FROM alerts WHERE id='alert-rj' AND tenant_id=?`, actTenant).Scan(&st)
	if st != alert.StatusFiring {
		t.Fatalf("拒绝后 alert 状态不应变化: %s", st)
	}

	// 过期 → 拒绝确认
	req3 := seedPendingAction(t, db, "ack_alert", map[string]any{"alert_id": "alert-ack"}, assistant.RiskLow)
	past := time.Now().UTC().Add(-time.Minute)
	if err := db.Model(req3).Update("expires_at", past).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmAction(ctx, actTenant, req3.ID, "local-noauth", ""); err == nil {
		t.Fatal("过期请求应拒绝确认")
	}
	db.Raw(`SELECT status FROM assistant_action_requests WHERE id=?`, req3.ID).Scan(&st)
	if st != assistant.ActionExpired {
		t.Fatalf("过期请求状态应置 expired: %s", st)
	}

	// 执行器未装配 → 报错
	svcNoExec := newActService(t, db, false)
	req4 := seedPendingAction(t, db, "ack_alert", map[string]any{"alert_id": "alert-ack"}, assistant.RiskLow)
	if _, err := svcNoExec.ConfirmAction(ctx, actTenant, req4.ID, "local-noauth", ""); err == nil {
		t.Fatal("未装配执行器应报错")
	}
}

func TestActionResolveIncident(t *testing.T) {
	db := openActDB(t)
	ctx := context.Background()
	seedActIncident(t, db, "inc-act-1", incident.StateOpen)

	svc := newActService(t, db, true)
	req := seedPendingAction(t, db, "resolve_incident", map[string]any{"incident_id": "inc-act-1"}, assistant.RiskMedium)
	got, err := svc.ConfirmAction(ctx, actTenant, req.ID, "local-noauth", "处置完成")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != assistant.ActionSucceeded {
		t.Fatalf("status = %s, want succeeded", got.Status)
	}
	var st string
	if err := db.Raw(`SELECT status FROM incidents WHERE id='inc-act-1' AND tenant_id=?`, actTenant).Scan(&st).Error; err != nil {
		t.Fatal(err)
	}
	if st != incident.IncidentResolved {
		t.Fatalf("incident status = %s, want resolved", st)
	}
}

func TestActionApproveIncidentAction(t *testing.T) {
	db := openActDB(t)
	ctx := context.Background()
	seedActIncident(t, db, "inc-act-pa", incident.StateOpen)
	seedPendingApprovalState(t, db, "inc-act-pa")

	svc := newActService(t, db, true)
	req := seedPendingAction(t, db, "approve_incident_action",
		map[string]any{"incident_id": "inc-act-pa", "decision": "approve"}, assistant.RiskHigh)
	got, err := svc.ConfirmAction(ctx, actTenant, req.ID, "local-noauth", "同意扩容")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != assistant.ActionSucceeded {
		t.Fatalf("status = %s, want succeeded: %s", got.Status, got.DecisionReason)
	}
	// 编排器闭环：state resolved + heal executed + incident resolved
	var st string
	db.Raw(`SELECT status FROM incident_states WHERE id='inc-act-pa' AND tenant_id=?`, actTenant).Scan(&st)
	if st != incident.StateResolved {
		t.Fatalf("state status = %s, want resolved", st)
	}
	var inc string
	db.Raw(`SELECT status FROM incidents WHERE id='inc-act-pa' AND tenant_id=?`, actTenant).Scan(&inc)
	if inc != incident.IncidentResolved {
		t.Fatalf("incident = %s, want resolved", inc)
	}
}

func TestRunnerProposesActionOnly(t *testing.T) {
	db := openDB(t)
	tenant := "asst-act-runner"
	cleanup(t, db, tenant)
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM assistant_action_requests WHERE tenant_id = ?", tenant).Error
		_ = db.Exec("DELETE FROM alerts WHERE tenant_id = ?", tenant).Error
	})
	seedActAlertT(t, db, tenant, "alert-run", alert.StatusFiring)

	model := &fakeModel{responses: []*llm.Completion{toolCallResp("ack_alert", `{"alert_id":"alert-run"}`)}}
	runner, _ := buildRunner(t, model, assistant.Limits{
		MaxConcurrency: 2, MaxModelRounds: 8, MaxToolCalls: 12,
		RunTimeout: 5 * time.Second, ToolTimeout: 3 * time.Second,
	})
	thread := newThread(t, db, tenant)
	run := newRun(t, db, tenant, thread.ID)
	runner.ProcessClaimed(context.Background(), run)

	// run 完成且仅提出请求（pending），未执行副作用
	var n int64
	if err := db.Raw(`SELECT count(*) FROM assistant_action_requests WHERE tenant_id=? AND run_id=? AND status='pending'`,
		tenant, run.ID).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应恰好 1 条 pending 请求: %d", n)
	}
	var st string
	if err := db.Raw(`SELECT status FROM alerts WHERE id='alert-run' AND tenant_id=?`, tenant).Scan(&st).Error; err != nil {
		t.Fatal(err)
	}
	if st != alert.StatusFiring {
		t.Fatalf("未确认前不应执行副作用: %s", st)
	}
}

func seedActAlertT(t *testing.T, db *gorm.DB, tenant, id, status string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO alerts (id, tenant_id, provider, provider_type, fingerprint, alert_hash, event,
		name, severity, status, labels, started_at, last_seen_at, created_at, updated_at)
		VALUES (?, ?, 'webhook', 'generic', ?, 'h', '{}'::jsonb, 'pay-down', 'critical', ?, '{}'::jsonb,
			now(), now(), now(), now())`, id, tenant, "fp-"+id, status).Error; err != nil {
		t.Fatal(err)
	}
}
