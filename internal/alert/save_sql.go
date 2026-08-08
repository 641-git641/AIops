package alert

import (
	"strings"
	"time"
)

// buildSaveSQL 构造「单语句双表写入」：alerts insert + last_alerts upsert（规模化的写入优化，
// 每事件 DB 往返 4 次（BEGIN/INSERT/UPSERT/COMMIT）→ 1 次，单语句原子语义不变）。
// 返回 SQL 与按出现顺序的参数；列清单是本文件唯一事实来源（新增模型字段需同步）。
func buildSaveSQL(a *Alert) (string, []any) {
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now // raw Exec 无 GORM Create 的自动回填，保持与旧路径一致
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = now
	}
	sql := `WITH ins AS (
    INSERT INTO alerts (id, tenant_id, provider, provider_type, fingerprint, alert_hash,
                        event, name, severity, status, labels,
                        started_at, last_seen_at, created_at, updated_at)
    VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?, ?, ?::jsonb, ?, ?, ?, ?)
    RETURNING id, tenant_id, fingerprint, alert_hash, status, last_seen_at
)
INSERT INTO last_alerts (id, tenant_id, fingerprint, alert_id, alert_hash, status, last_seen_at, created_at, updated_at)
SELECT ?, tenant_id, fingerprint, id, alert_hash, status, last_seen_at, ?, ?
FROM ins
ON CONFLICT (tenant_id, fingerprint) DO UPDATE SET
    alert_id = EXCLUDED.alert_id,
    alert_hash = EXCLUDED.alert_hash,
    status = EXCLUDED.status,
    last_seen_at = EXCLUDED.last_seen_at,
    updated_at = EXCLUDED.updated_at`
	// 注意：datatypes.JSON 底层是 []byte，直接绑定会被 pgx 编码成 bytea；
	// 必须转 string 走 ?::jsonb（text→jsonb 隐式 cast）。
	args := []any{
		a.ID, a.TenantID, a.Provider, a.ProviderType, a.Fingerprint, a.AlertHash,
		string(a.Event), a.Name, a.Severity, a.Status, string(a.Labels),
		a.StartedAt, a.LastSeenAt, a.CreatedAt, a.UpdatedAt,
		newID(), a.CreatedAt, a.UpdatedAt,
	}
	return strings.TrimSpace(sql), args
}
