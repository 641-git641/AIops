package anomaly_test

import (
	"context"
	"testing"
	"time"

	"aiops/internal/anomaly"
	"aiops/internal/store"

	"gorm.io/gorm"
)

// DB 门控集成测试：插入已知模式数据 → 验证基线计算与异常标记。

const anomTenant = "anomaly-it"

func openAnomalyDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(store.DefaultDSN, store.Options{})
	if err != nil {
		t.Skipf("postgres 未运行（docker compose），跳过: %v", err)
	}
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS alerts (
		id varchar(36) PRIMARY KEY, tenant_id varchar(64), provider varchar(64),
		provider_type varchar(64), fingerprint varchar(64), alert_hash varchar(64),
		event jsonb, name varchar(255), severity varchar(16), status varchar(16),
		labels jsonb, started_at timestamptz, last_seen_at timestamptz,
		created_at timestamptz, updated_at timestamptz)`).Error; err != nil {
		t.Fatalf("ensure alerts table: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM alerts WHERE tenant_id = ?", anomTenant).Error
	})
	return db
}

// seedHourly 在指定小时整点插入 n 条告警。
func seedHourly(t *testing.T, db *gorm.DB, hour time.Time, n int) {
	t.Helper()
	// 批量 INSERT（generate_series 简化）
	if err := db.Exec(`
		INSERT INTO alerts (id, tenant_id, provider, provider_type, fingerprint, alert_hash, event,
			name, severity, status, labels, started_at, last_seen_at, created_at, updated_at)
		SELECT 'an-' || tenant_id || '-' || g, tenant_id, 'webhook', 'generic', 'fp-' || g, 'h', '{}'::jsonb,
			'seed', 'low', 'firing', '{}'::jsonb, ?, ?, ?, ?
		FROM generate_series(1, ?) AS g
		CROSS JOIN (SELECT ? AS tenant_id) t`,
		hour, hour, hour, hour, n, anomTenant).Error; err != nil {
		t.Fatal(err)
	}
}

func TestAlertAnomaliesBaselineAndFlag(t *testing.T) {
	db := openAnomalyDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Hour)
	// 过去 7 天同一小时：每天 2 条（稳定基线）
	for d := 1; d <= 7; d++ {
		seedHourly(t, db, now.Add(-time.Duration(d)*24*time.Hour), 2)
	}
	// 当前小时：60 条（远超基线 2±σ）
	seedHourly(t, db, now, 60)

	svc := anomaly.NewService(db)
	items, err := svc.AlertAnomalies(ctx, anomTenant, anomaly.Options{Hours: 24, HistoryDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 24 {
		t.Fatalf("应返回 24 小时: %d", len(items))
	}
	// 最后一项 = 当前小时：count=60，样本充足（7 天）→ anomaly
	cur := items[len(items)-1]
	if cur.Count != 60 {
		t.Fatalf("当前小时 count = %d, want 60", cur.Count)
	}
	if !cur.Anomaly {
		t.Fatalf("当前小时应标记异常: %+v", cur)
	}
	if cur.Baseline != 2 {
		t.Fatalf("基线应=2（7 天同小时均值）: %+v", cur)
	}
	// 其他小时：无告警（count=0）→ 无论样本多少都不超上限 → 非异常
	other := items[len(items)-2]
	if other.Count != 0 || other.Anomaly {
		t.Fatalf("前一小时不应异常: %+v", other)
	}
}

func TestAlertAnomaliesInsufficientHistory(t *testing.T) {
	db := openAnomalyDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	seedHourly(t, db, now, 5) // 只有当前小时数据

	svc := anomaly.NewService(db)
	items, err := svc.AlertAnomalies(ctx, anomTenant, anomaly.Options{Hours: 2, HistoryDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	cur := items[len(items)-1]
	if cur.Anomaly {
		t.Fatalf("样本不足不应标记异常: %+v", cur)
	}
	if cur.Reason == "" || !containsStr(cur.Reason, "样本不足") {
		t.Fatalf("应提示样本不足: %+v", cur)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
