package alert

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"aiops/internal/util"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Repository 告警仓储：负责 alerts/last_alerts 的读写与去重热路径。
type Repository struct {
	db *gorm.DB
}

// NewRepository 创建仓储。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// SaveAlert 在单事务中写入 alerts 并 upsert last_alerts。
// kind 为去重分类结果：new/update 写入，duplicate 跳过。
func (r *Repository) SaveAlert(ctx context.Context, a *Alert, kind DedupKind) error {
	if kind == DedupDuplicate {
		return nil // 完全重复：不入库
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(a).Error; err != nil {
			return err
		}
		return r.upsertLastAlert(tx, a)
	})
}

// UpsertLastAlert 单独刷新状态缓存（如规则引擎、确认操作时）。
func (r *Repository) UpsertLastAlert(ctx context.Context, a *Alert) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return r.upsertLastAlert(tx, a)
	})
}

func (r *Repository) upsertLastAlert(tx *gorm.DB, a *Alert) error {
	la := LastAlert{
		ID:          util.NewID(),
		TenantID:    a.TenantID,
		Fingerprint: a.Fingerprint,
		AlertID:     a.ID,
		Status:      a.Status,
		LastSeenAt:  a.LastSeenAt,
	}
	// 按 (tenant_id, fingerprint) 唯一索引 upsert
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "fingerprint"}},
		DoUpdates: clause.AssignmentColumns([]string{"alert_id", "status", "last_seen_at", "updated_at"}),
	}).Create(&la).Error
}

// GetLastAlert 按 (tenant, fingerprint) 查询状态缓存。
func (r *Repository) GetLastAlert(ctx context.Context, tenantID, fingerprint string) (*LastAlert, error) {
	var la LastAlert
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND fingerprint = ?", tenantID, fingerprint).First(&la).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &la, nil
}

// Query 告警列表查询参数。
type Query struct {
	TenantID    string
	Status      string
	Severity    string
	Provider    string
	Fingerprint string
	From        *time.Time
	To          *time.Time
	Limit       int
	Offset      int
}

// List 查询告警列表（按 created_at 倒序）。
func (r *Repository) List(ctx context.Context, q Query) ([]Alert, int64, error) {
	stmt := r.db.WithContext(ctx).Model(&Alert{})
	stmt = stmt.Where("tenant_id = ?", q.TenantID)
	if q.Status != "" {
		stmt = stmt.Where("status = ?", q.Status)
	}
	if q.Severity != "" {
		stmt = stmt.Where("severity = ?", q.Severity)
	}
	if q.Provider != "" {
		stmt = stmt.Where("provider = ?", q.Provider)
	}
	if q.Fingerprint != "" {
		stmt = stmt.Where("fingerprint = ?", q.Fingerprint)
	}
	if q.From != nil {
		stmt = stmt.Where("created_at >= ?", *q.From)
	}
	if q.To != nil {
		stmt = stmt.Where("created_at <= ?", *q.To)
	}
	var total int64
	if err := stmt.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var items []Alert
	err := stmt.Order("created_at DESC").Limit(limit).Offset(q.Offset).Find(&items).Error
	return items, total, err
}

// Get 按 ID 查询告警。
func (r *Repository) Get(ctx context.Context, tenantID, id string) (*Alert, error) {
	var a Alert
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&a).Error
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// UpdateStatus 更新告警状态并同步 last_alerts 缓存（确认/恢复操作）。
func (r *Repository) UpdateStatus(ctx context.Context, tenantID, id, status string) (*Alert, error) {
	a, err := r.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	a.Status = status
	a.UpdatedAt = time.Now().UTC()
	if err := r.db.WithContext(ctx).Model(a).Update("status", status).Error; err != nil {
		return nil, err
	}
	return a, r.UpsertLastAlert(ctx, a)
}

// History 按 fingerprint 查询历史（去重演进轨迹）。
func (r *Repository) History(ctx context.Context, tenantID, fingerprint string, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var items []Alert
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND fingerprint = ?", tenantID, fingerprint).
		Order("created_at DESC").Limit(limit).Find(&items).Error
	return items, err
}

// -------- helpers --------

func newID() string { return util.NewID() }

func mustJSON(v any) datatypes.JSON {
	b, err := json.Marshal(v)
	if err != nil {
		return datatypes.JSON(`{}`)
	}
	return datatypes.JSON(b)
}

func normalizeSeverity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo:
		return s
	case "p0", "severe", "error":
		if s == "error" {
			return SeverityHigh
		}
		return SeverityCritical
	case "warning", "warn":
		return SeverityMedium
	case "notice":
		return SeverityInfo
	}
	return SeverityMedium
}

func normalizeStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "firing", "active", "triggered", "open", "pending":
		return StatusFiring
	case "resolved", "ok", "recovered", "closed", "fired":
		return StatusResolved
	case "acknowledged", "ack":
		return StatusAcknowledged
	case "suppressed":
		return StatusSuppressed
	}
	return StatusFiring
}
