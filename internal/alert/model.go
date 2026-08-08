// Package alert 定义告警领域模型（Alert/LastAlert）与仓储。
package alert

import (
	"encoding/json"
	"time"

	"gorm.io/datatypes"
)

// Severity 严重度。
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
	SeverityInfo     = "info"
)

// SeverityRank 严重度数值（用于比较与阈值过滤），未知返回 0。
func SeverityRank(s string) int {
	switch s {
	case SeverityCritical:
		return 4
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	case SeverityInfo:
		return 0
	}
	return 0
}

// Status 告警状态。
const (
	StatusFiring       = "firing"       // 触发中
	StatusResolved     = "resolved"     // 已恢复
	StatusAcknowledged = "acknowledged" // 已确认（认领）
	StatusSuppressed   = "suppressed"   // 被维护窗口静默
)

// DedupKind 去重分类结果。
type DedupKind string

const (
	DedupNew       DedupKind = "new"       // 新告警
	DedupUpdate    DedupKind = "update"    // 部分重复：状态/内容变化，刷新
	DedupDuplicate DedupKind = "duplicate" // 完全重复：丢弃
)

// Alert 告警全量历史记录（无唯一约束，允许同 fingerprint 多条）。
type Alert struct {
	ID           string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID     string         `gorm:"size:64;index:idx_alerts_tenant_created,priority:1" json:"tenantId"`
	Provider     string         `gorm:"size:64" json:"provider"`                                               // 接入渠道：prometheus/aliyun/...
	ProviderType string         `gorm:"size:64" json:"providerType"`                                           // 源系统名
	Fingerprint  string         `gorm:"size:64;index:idx_alerts_tenant_created,priority:4" json:"fingerprint"` // 逻辑去重键
	AlertHash    string         `gorm:"size:64" json:"alertHash"`                                              // 物理去重键（内容哈希）
	Event        datatypes.JSON `json:"event"`                                                                 // 原始事件快照（供 CEL/模板/AI 使用）
	Name         string         `gorm:"size:255" json:"name"`
	Severity     string         `gorm:"size:16;index:idx_alerts_tenant_created,priority:2" json:"severity"`
	Status       string         `gorm:"size:16;index:idx_alerts_status" json:"status"`
	Labels       datatypes.JSON `json:"labels"`
	StartedAt    time.Time      `json:"startedAt"`
	LastSeenAt   time.Time      `json:"lastSeenAt"`
	CreatedAt    time.Time      `gorm:"index:idx_alerts_created" json:"createdAt"` // 保留策略分批清理按此列删除
	UpdatedAt    time.Time      `json:"updatedAt"`
}

// TableName 表名。
func (Alert) TableName() string { return "alerts" }

// LastAlert 告警当前状态缓存（去重热路径），(tenant_id, fingerprint) 唯一。
// alert_hash 冗余存储：与 status 一起保证 dedup 只读本表唯一索引 1 次即完成
// 分类（不再回读 alerts 分区表），写入路径（SaveAlert CTE / UpsertLastAlert）同步维护。
type LastAlert struct {
	ID          string    `gorm:"primaryKey;size:36" json:"id"`
	TenantID    string    `gorm:"size:64;uniqueIndex:uk_lastalert_tenant_fp,priority:1" json:"tenantId"`
	Fingerprint string    `gorm:"size:64;uniqueIndex:uk_lastalert_tenant_fp,priority:2" json:"fingerprint"`
	AlertID     string    `gorm:"size:36" json:"alertId"`   // 当前活跃告警引用
	AlertHash   string    `gorm:"size:64" json:"alertHash"` // 物理去重键（内容哈希）冗余
	Status      string    `gorm:"size:16" json:"status"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// TableName 表名。
func (LastAlert) TableName() string { return "last_alerts" }

// IngestEvent 是 provider 归一化后提交给管线的入站事件。
type IngestEvent struct {
	TenantID     string         `json:"tenantId"`
	Provider     string         `json:"provider"` // 必须指定接入渠道（prometheus 等）
	ProviderType string         `json:"providerType,omitempty"`
	Name         string         `json:"name"`
	Severity     string         `json:"severity"`
	Status       string         `json:"status"`
	Labels       map[string]any `json:"labels,omitempty"`
	Event        map[string]any `json:"event,omitempty"` // 原始快照（可为 nil，回退用 Name/Labels）
	StartedAt    *time.Time     `json:"startedAt,omitempty"`
}

// BuildAlert 由 IngestEvent 计算 fingerprint / alert_hash 并构造 Alert。
// fingerprint 逻辑键：name + provider；可通过 labels 中的 fingerprintFields 扩展。
func (e *IngestEvent) BuildAlert() *Alert {
	now := time.Now().UTC()
	started := now
	if e.StartedAt != nil {
		started = *e.StartedAt
	}
	labels := e.Labels
	if labels == nil {
		labels = map[string]any{}
	}
	event := e.Event
	if event == nil {
		// 原始快照缺失时用结构化字段兜底
		event = map[string]any{"name": e.Name, "severity": e.Severity, "status": e.Status, "labels": labels}
	}
	fp := Fingerprint(e.Provider, e.Name, e.Labels)
	return &Alert{
		ID:           newID(),
		TenantID:     e.TenantID,
		Provider:     e.Provider,
		ProviderType: e.ProviderType,
		Fingerprint:  fp,
		AlertHash:    ContentHash(event),
		Name:         e.Name,
		Severity:     normalizeSeverity(e.Severity),
		Status:       normalizeStatus(e.Status),
		Labels:       mustJSON(labels),
		Event:        mustJSON(event),
		StartedAt:    started,
		LastSeenAt:   now,
	}
}

// DTO 输出用的告警视图（避免暴露内部字段）。
type DTO struct {
	ID          string         `json:"id"`
	Provider    string         `json:"provider"`
	Name        string         `json:"name"`
	Severity    string         `json:"severity"`
	Status      string         `json:"status"`
	Labels      map[string]any `json:"labels"`
	Event       map[string]any `json:"event"`
	Fingerprint string         `json:"fingerprint"`
	StartedAt   time.Time      `json:"startedAt"`
	LastSeenAt  time.Time      `json:"lastSeenAt"`
}

// ToDTO 转换为对外 DTO。
func (a *Alert) ToDTO() DTO {
	labels := map[string]any{}
	_ = json.Unmarshal(a.Labels, &labels)
	event := map[string]any{}
	_ = json.Unmarshal(a.Event, &event)
	return DTO{
		ID: a.ID, Provider: a.Provider, Name: a.Name, Severity: a.Severity,
		Status: a.Status, Labels: labels, Event: event, Fingerprint: a.Fingerprint,
		StartedAt: a.StartedAt, LastSeenAt: a.LastSeenAt,
	}
}
