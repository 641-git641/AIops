package eventbus

import (
	"context"
	"time"

	"aiops/internal/util"

	"gorm.io/gorm"
)

// AuditService 审计日志落库服务（审批/处置的合规基线，who/when/what/why）。
// 事件总线上的 audit topic 是扩散/订阅用的瞬时事件；这里负责持久化到 audit_logs 表。
type AuditService struct {
	db *gorm.DB
}

// NewAuditService 创建审计服务。
func NewAuditService(db *gorm.DB) *AuditService {
	return &AuditService{db: db}
}

// Record 记录一条审计（返回记录 ID）。
func (s *AuditService) Record(ctx context.Context, in AuditLog) (string, error) {
	if in.ID == "" {
		in.ID = util.NewID()
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now().UTC()
	}
	if err := s.db.WithContext(ctx).Create(&in).Error; err != nil {
		return "", err
	}
	return in.ID, nil
}

// List 按 incident 查询审计记录（倒序）。
func (s *AuditService) List(ctx context.Context, tenantID, incidentID string, limit, offset int) ([]AuditLog, int64, error) {
	stmt := s.db.WithContext(ctx).Model(&AuditLog{}).
		Where("tenant_id = ?", tenantID)
	if incidentID != "" {
		stmt = stmt.Where("incident_id = ?", incidentID)
	}
	var total int64
	if err := stmt.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var items []AuditLog
	err := stmt.Order("created_at DESC").Limit(limit).Offset(offset).Find(&items).Error
	return items, total, err
}
