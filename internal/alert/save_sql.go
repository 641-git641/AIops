package alert

import (
	"sort"
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

// lastAlertKey 批量写折叠键（(tenant, fingerprint) 与 last_alerts 唯一索引一致）。
type lastAlertKey struct {
	tenantID    string
	fingerprint string
}

// buildSaveAlertsSQL 构造「单语句多行双表写入」（SaveAlerts 批量写，Phase 3g）：
// alerts 全量行 insert + last_alerts 折叠 upsert，一次 DB 往返，单语句原子。
//
// 折叠原因：单语句内多行同 (tenant_id, fingerprint) 的 ON CONFLICT DO UPDATE 会触发
// PG「cannot affect row a second time」；last_alerts 是当前状态真源（每 fingerprint
// 只保留批内最后一条的最终状态），而 alerts 历史表每事件都是一条独立记录（全量保留）。
//
// kinds 与 alerts 一一对应：duplicate 跳过（对齐 SaveAlert 语义）。返回 SQL 与
// 按出现顺序的参数；列清单是本文件唯一事实来源（新增模型字段需同步）。
func buildSaveAlertsSQL(alerts []*Alert, kinds []DedupKind) (string, []any) {
	now := time.Now().UTC()
	keep := make([]*Alert, 0, len(alerts))
	for i, a := range alerts {
		if kinds[i] == DedupDuplicate {
			continue
		}
		if a.CreatedAt.IsZero() {
			a.CreatedAt = now // raw Exec 无 GORM Create 的自动回填，与 buildSaveSQL 一致
		}
		if a.UpdatedAt.IsZero() {
			a.UpdatedAt = now
		}
		keep = append(keep, a)
	}
	if len(keep) == 0 {
		return "", nil
	}
	// 折叠：每 (tenant_id, fingerprint) 只保留批内最后一条（alerts 全量、last_alerts 单态）
	lastIdx := make(map[lastAlertKey]int, len(keep))
	for i, a := range keep {
		lastIdx[lastAlertKey{a.TenantID, a.Fingerprint}] = i
	}
	collapsed := make([]int, 0, len(lastIdx))
	for _, i := range lastIdx {
		collapsed = append(collapsed, i)
	}
	sort.Ints(collapsed) // 确定性顺序（VALUES 与 args 同序）

	var b strings.Builder
	b.WriteString(`WITH ins AS (
    INSERT INTO alerts (id, tenant_id, provider, provider_type, fingerprint, alert_hash,
                        event, name, severity, status, labels,
                        started_at, last_seen_at, created_at, updated_at)
    VALUES `)
	for i := range keep {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("(?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?, ?, ?::jsonb, ?, ?, ?, ?)")
	}
	b.WriteString(`
    RETURNING id
),
ups AS (
    INSERT INTO last_alerts (id, tenant_id, fingerprint, alert_id, alert_hash, status, last_seen_at, created_at, updated_at)
    VALUES `)
	for j := range collapsed {
		if j > 0 {
			b.WriteString(",")
		}
		b.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?)")
	}
	b.WriteString(`
    ON CONFLICT (tenant_id, fingerprint) DO UPDATE SET
        alert_id = EXCLUDED.alert_id,
        alert_hash = EXCLUDED.alert_hash,
        status = EXCLUDED.status,
        last_seen_at = EXCLUDED.last_seen_at,
        updated_at = EXCLUDED.updated_at
)
SELECT 1`)

	args := make([]any, 0, len(keep)*15+len(collapsed)*9)
	for _, a := range keep {
		// 注意：datatypes.JSON 底层是 []byte，直接绑定会被 pgx 编码成 bytea；
		// 必须转 string 走 ?::jsonb（text→jsonb 隐式 cast）。
		args = append(args,
			a.ID, a.TenantID, a.Provider, a.ProviderType, a.Fingerprint, a.AlertHash,
			string(a.Event), a.Name, a.Severity, a.Status, string(a.Labels),
			a.StartedAt, a.LastSeenAt, a.CreatedAt, a.UpdatedAt,
		)
	}
	for _, i := range collapsed {
		a := keep[i]
		args = append(args,
			newID(), a.TenantID, a.Fingerprint, a.ID, a.AlertHash, a.Status,
			a.LastSeenAt, a.CreatedAt, a.UpdatedAt,
		)
	}
	return strings.TrimSpace(b.String()), args
}
