package maintenance

import (
	"context"
	"encoding/json"
	"time"

	"aiops/internal/alert"
	"aiops/internal/util"

	"gorm.io/gorm"
)

// Service 维护窗口仓储 + 匹配逻辑。
type Service struct {
	db *gorm.DB
}

// NewService 创建维护窗口服务。
func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// Create 创建维护窗口。
func (s *Service) Create(ctx context.Context, w *Window) error {
	if w.ID == "" {
		w.ID = util.NewID()
	}
	now := time.Now().UTC()
	if w.CreatedAt.IsZero() {
		w.CreatedAt = now
	}
	w.UpdatedAt = now
	return s.db.WithContext(ctx).Create(w).Error
}

// List 返回某租户全部维护窗口（按开始时间倒序）。
func (s *Service) List(ctx context.Context, tenantID string) ([]Window, error) {
	var items []Window
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).
		Order("starts_at DESC").Find(&items).Error
	return items, err
}

// Get 按 ID 查询维护窗口。
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Window, error) {
	var w Window
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&w).Error
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// Update 更新维护窗口（全字段覆盖）。
func (s *Service) Update(ctx context.Context, w *Window) error {
	w.UpdatedAt = time.Now().UTC()
	updates := map[string]any{
		"name": w.Name, "fingerprint": w.Fingerprint, "service": w.Service,
		"severity": w.Severity, "starts_at": w.StartsAt, "ends_at": w.EndsAt,
		"enabled": w.Enabled, "reason": w.Reason, "updated_at": w.UpdatedAt,
	}
	return s.db.WithContext(ctx).Model(&Window{}).
		Where("tenant_id = ? AND id = ?", w.TenantID, w.ID).Updates(updates).Error
}

// Delete 删除维护窗口。
func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&Window{}).Error
}

// ActiveWindows 返回某租户当前生效（enabled 且 now ∈ [starts, ends)）的维护窗口。
func (s *Service) ActiveWindows(ctx context.Context, tenantID string, now time.Time) ([]Window, error) {
	var items []Window
	err := s.db.WithContext(ctx).Where(
		"tenant_id = ? AND enabled = ? AND starts_at <= ? AND ends_at > ?",
		tenantID, true, now, now,
	).Find(&items).Error
	return items, err
}

// Matches 判定告警是否命中维护窗口。
// 规则：仅 firing 告警可被静默（resolved 一律放行，保证恢复链路不被吞）；
// 窗口至少配置一个匹配条件，且所有已配置条件必须全部满足。
func (s *Service) Matches(w *Window, a *alert.Alert) bool {
	if a.Status != alert.StatusFiring {
		return false
	}
	hasCriterion := false
	if w.Fingerprint != "" {
		hasCriterion = true
		if w.Fingerprint != a.Fingerprint {
			return false
		}
	}
	if w.Service != "" {
		hasCriterion = true
		if labelService(a) != w.Service {
			return false
		}
	}
	if w.Severity != "" {
		hasCriterion = true
		// 窗口覆盖 <= 指定严重度的告警（如 severity=low 只静默 low/info）
		if alert.SeverityRank(a.Severity) > alert.SeverityRank(w.Severity) {
			return false
		}
	}
	return hasCriterion
}

// labelService 从告警 labels 中提取 service（与指纹/规则引擎的取值约定一致）。
func labelService(a *alert.Alert) string {
	var labels map[string]any
	if err := json.Unmarshal(a.Labels, &labels); err != nil {
		return ""
	}
	if v, ok := labels["service"].(string); ok {
		return v
	}
	return ""
}
