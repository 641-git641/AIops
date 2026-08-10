package overview_test

import (
	"context"
	"testing"
	"time"

	"aiops/internal/alert"
	"aiops/internal/eventbus"
	"aiops/internal/overview"
	"aiops/internal/store"

	"gorm.io/gorm"
)

// openDB PostgreSQL 可用则连库，否则跳过（沿用 rag 集成测试模式）。
func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(store.DefaultDSN, store.Options{})
	if err != nil {
		t.Skipf("postgres 未运行（docker compose），跳过: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM alerts WHERE tenant_id = 'overview-test'").Error
		_ = db.Exec("DELETE FROM last_alerts WHERE tenant_id = 'overview-test'").Error
		_ = db.Exec("DELETE FROM incidents WHERE tenant_id = 'overview-test'").Error
		_ = db.Exec("DELETE FROM audit_logs WHERE tenant_id = 'overview-test'").Error
	})
	return db
}

const testTenant = "overview-test"

// saveAlert 造一条指定严重度/状态的告警。
func saveAlert(t *testing.T, db *gorm.DB, name, sev, status string, createdAt time.Time) {
	t.Helper()
	e := &alert.IngestEvent{
		TenantID: testTenant, Provider: "overview-test", Name: name,
		Severity: sev, Status: status,
		Labels:    map[string]any{"service": "svc-1"},
		StartedAt: &createdAt,
	}
	a := e.BuildAlert()
	a.CreatedAt = createdAt
	a.LastSeenAt = createdAt
	if err := db.Create(a).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(
		`INSERT INTO last_alerts (id, tenant_id, fingerprint, alert_id, status, alert_hash, last_seen_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (tenant_id, fingerprint) DO UPDATE SET status = EXCLUDED.status, last_seen_at = EXCLUDED.last_seen_at`,
		a.ID, testTenant, a.Fingerprint, a.ID, status, a.AlertHash, createdAt, createdAt, createdAt).Error; err != nil {
		t.Fatal(err)
	}
}

func TestOverviewAggregation(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// 跨小时 + 跨严重度造数：2 条 firing（1 critical + 1 medium），1 条 resolved
	saveAlert(t, db, "磁盘告警", "critical", "firing", now.Add(-2*time.Hour))
	saveAlert(t, db, "内存告警", "medium", "firing", now.Add(-1*time.Hour))
	saveAlert(t, db, "已恢复告警", "high", "resolved", now.Add(-30*time.Minute))
	// 审计事件
	audit := eventbus.NewAuditService(db)
	_, _ = audit.Record(ctx, eventbus.AuditLog{
		TenantID: testTenant, Actor: "auto-system", Action: "heal.executed",
		Target: "incident.x", Reason: "测试",
	})
	// 备份服务缺席 → BackupEnabled=false
	sqlDB, _ := db.DB()
	svc := overview.New(db, sqlDB, nil, nil, nil, now.Add(-time.Hour))

	ov, err := svc.Get(ctx, testTenant, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Counts.ActiveAlerts != 2 {
		t.Fatalf("active alerts = %d, want 2", ov.Counts.ActiveAlerts)
	}
	if ov.SeverityDist["critical"] != 1 || ov.SeverityDist["medium"] != 1 || ov.SeverityDist["high"] != 1 {
		t.Fatalf("severity dist wrong: %+v", ov.SeverityDist)
	}
	// 趋势：窗口 24h，应含数据小时且补零正确（桶数 = 24+1）
	if len(ov.Trend) < 24 {
		t.Fatalf("trend len = %d, want >= 24", len(ov.Trend))
	}
	var total int64
	for _, p := range ov.Trend {
		total += p.Count
	}
	if total != 3 {
		t.Fatalf("trend total = %d, want 3", total)
	}
	// 最近事件：告警 + 审计合并，倒序
	if len(ov.RecentEvents) < 2 {
		t.Fatalf("recent events = %d, want >= 2", len(ov.RecentEvents))
	}
	hasAlert, hasAudit := false, false
	for _, e := range ov.RecentEvents {
		if e.Kind == "alert" {
			hasAlert = true
		}
		if e.Kind == "audit" && e.Action == "heal.executed" {
			hasAudit = true
		}
	}
	if !hasAlert || !hasAudit {
		t.Fatalf("merge missing alert/audit: %+v", ov.RecentEvents)
	}
	for i := 1; i < len(ov.RecentEvents); i++ {
		if ov.RecentEvents[i].CreatedAt.After(ov.RecentEvents[i-1].CreatedAt) {
			t.Fatal("recent events not descending")
		}
	}
	// 系统健康：DB ok + uptime ≈1h
	if !ov.System.DBOk {
		t.Fatalf("db ping failed: %s", ov.System.DBError)
	}
	if ov.System.UptimeSec < 3000 || ov.System.UptimeSec > 4200 {
		t.Fatalf("uptime = %d, want ~3600", ov.System.UptimeSec)
	}
	// 备份未装配
	if ov.BackupEnabled || ov.LatestBackup != nil {
		t.Fatal("backup must be disabled when bk nil")
	}
	// 窗口过滤：30min 窗口只含 2 条（resolved 那条在窗口外…… 30min 前 + 边界）
	ov2, err := svc.Get(ctx, testTenant, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var total2 int64
	for _, p := range ov2.Trend {
		total2 += p.Count
	}
	if total2 > 2 {
		t.Fatalf("window filter failed: total = %d", total2)
	}
}
