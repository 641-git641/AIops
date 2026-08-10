package incident

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"aiops/internal/util"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 结案数据契约（RAG_PLAN §5.1）：一对一记录人工确认根因与处置结果。
// 只有 verification_status=verified 的事故可作为高可信证据（T3）；
// 未验证事故仅作低权重相似性参考；rejected 退出召回。
type Resolution struct {
	ID                 string    `gorm:"primaryKey;size:36" json:"id"`
	TenantID           string    `gorm:"size:64;uniqueIndex:uk_res_tenant_incident,priority:1" json:"tenantId"`
	IncidentID         string    `gorm:"size:36;uniqueIndex:uk_res_tenant_incident,priority:2" json:"incidentId"`
	ConfirmedRootCause string    `gorm:"type:text" json:"confirmedRootCause"` // 人工确认根因
	Resolution         string    `gorm:"type:text" json:"resolution"`         // 实际处置动作
	Outcome            string    `gorm:"size:16" json:"outcome"`              // success|partial|failed|false_positive
	Verification       string    `gorm:"type:text" json:"verification"`       // 验证方法及结果
	VerificationStatus string    `gorm:"size:16" json:"verificationStatus"`   // verified|unverified|rejected
	PostmortemURL      string    `gorm:"size:512" json:"postmortemUrl,omitempty"`
	ResolvedBy         string    `gorm:"size:64" json:"resolvedBy,omitempty"`
	ResolvedAt         time.Time `json:"resolvedAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
	Version            int64     `json:"version"` // 知识版本：每次更新 +1（索引以此为 source_version）
}

// TableName 表名。
func (Resolution) TableName() string { return "incident_resolutions" }

// 处置结果。
const (
	OutcomeSuccess       = "success"
	OutcomePartial       = "partial"
	OutcomeFailed        = "failed"
	OutcomeFalsePositive = "false_positive"
)

// 验证状态。
const (
	VerifVerified   = "verified"
	VerifUnverified = "unverified"
	VerifRejected   = "rejected"
)

// IndexHook 知识索引钩子（RAG 装配层注入；nil 时不进事务、行为与现有完全一致）。
// 在源数据更新事务内调用：tx 是同一事务，钩子写 knowledge_jobs 保证最终一致。
type IndexHook func(ctx context.Context, tx *gorm.DB, tenantID, sourceID string, sourceVersion int64, op string) error

// UpsertResolution 写入/更新结案数据（一对一幂等；version 递增；索引钩子同事务入队）。
// 校验：verified 必须带确认根因与处置结果；枚举非法返回错误。
func (s *Service) UpsertResolution(ctx context.Context, tenantID, incidentID string, r *Resolution) error {
	if err := ValidateResolution(r); err != nil {
		return err
	}
	r.TenantID, r.IncidentID = tenantID, incidentID // 以租户上下文为准，覆盖客户端传入
	if s.resHook == nil {
		return s.upsertResolutionDB(ctx, s.db, r)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := s.upsertResolutionDB(ctx, tx, r); err != nil {
			return err
		}
		// 同事务入队：RAG 索引 worker 消费。钩子失败仅告警不阻塞源数据提交
		// （reconciliation 会按 version 失步兜底补单）
		if err := s.resHook(ctx, tx, tenantID, incidentID, r.Version, "upsert"); err != nil {
			slog.Warn("incident: enqueue rag index job", "incident", incidentID, "err", err)
		}
		return nil
	})
}

// upsertResolutionDB 纯 DB 更新：version = 旧值+1（SELECT FOR UPDATE 防并发双写）。
func (s *Service) upsertResolutionDB(ctx context.Context, db *gorm.DB, r *Resolution) error {
	if r.ID == "" {
		r.ID = util.NewID()
	}
	now := time.Now().UTC()
	r.UpdatedAt = now
	var old Resolution
	err := db.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND incident_id = ?", r.TenantID, r.IncidentID).First(&old).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		r.Version = 1
		if r.ResolvedAt.IsZero() {
			r.ResolvedAt = now
		}
		return db.WithContext(ctx).Create(r).Error
	case err != nil:
		return err
	}
	r.ID = old.ID
	r.Version = old.Version + 1
	if !old.ResolvedAt.IsZero() {
		r.ResolvedAt = old.ResolvedAt // 首次记录时间不变，更新只刷新内容
	}
	return db.WithContext(ctx).Model(&Resolution{}).Where("tenant_id = ? AND incident_id = ?", r.TenantID, r.IncidentID).
		Updates(map[string]any{
			"confirmed_root_cause": r.ConfirmedRootCause,
			"resolution":           r.Resolution,
			"outcome":              r.Outcome,
			"verification":         r.Verification,
			"verification_status":  r.VerificationStatus,
			"postmortem_url":       r.PostmortemURL,
			"resolved_by":          r.ResolvedBy,
			"resolved_at":          r.ResolvedAt,
			"updated_at":           r.UpdatedAt,
			"version":              r.Version,
		}).Error
}

// ValidateResolution 结案数据校验（枚举 + verified 数据完整性）。
func ValidateResolution(r *Resolution) error {
	switch r.Outcome {
	case OutcomeSuccess, OutcomePartial, OutcomeFailed, OutcomeFalsePositive:
	default:
		return fmt.Errorf("outcome 必须为 success|partial|failed|false_positive，got %q", r.Outcome)
	}
	switch r.VerificationStatus {
	case VerifVerified, VerifUnverified, VerifRejected:
	default:
		return fmt.Errorf("verificationStatus 必须为 verified|unverified|rejected，got %q", r.VerificationStatus)
	}
	if r.VerificationStatus == VerifVerified && (r.ConfirmedRootCause == "" || r.Resolution == "") {
		return errors.New("verificationStatus=verified 必须填写确认根因与处置结果（数据质量：T3 证据要求完整闭环）")
	}
	return nil
}

// GetResolution 查询结案数据（不存在返回 nil, nil）。
func (s *Service) GetResolution(ctx context.Context, tenantID, incidentID string) (*Resolution, error) {
	var r Resolution
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND incident_id = ?", tenantID, incidentID).First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}
