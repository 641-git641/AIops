package incident

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"aiops/internal/alert"
	"aiops/internal/metrics"
	"aiops/internal/util"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// IncidentNotify 事故生命周期通知回调（装配层注入；nil 跳过）。
// action 取值：created（新建事故）/ resolved（事故解决）。
type IncidentNotify func(ctx context.Context, inc *Incident, action string)

// Service Incident 领域服务。
type Service struct {
	db      *gorm.DB
	notify  IncidentNotify    // 生命周期通知（可选，nil 跳过）
	resHook IndexHook         // 知识索引钩子（RAG 装配层注入；nil 时不进事务，行为与现有完全一致）
	m       *metrics.Registry // 指标注册表（Phase 3 压测观测；nil 跳过埋点）
}

// NewService 创建 Incident 服务。
func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// SetNotifyFunc 注入事故生命周期通知回调（新建/解决时触发，fail-open）。
func (s *Service) SetNotifyFunc(fn IncidentNotify) { s.notify = fn }

// SetResIndexHook 注入结案/解决知识索引钩子（RAG 装配层；nil 保持现有行为）。
func (s *Service) SetResIndexHook(fn IndexHook) { s.resHook = fn }

// SetMetrics 装配指标注册表（Phase 3 压测观测；nil 关闭埋点）。
func (s *Service) SetMetrics(m *metrics.Registry) { s.m = m }

// Create 创建 incident，并触发 created 生命周期通知。
func (s *Service) Create(ctx context.Context, inc *Incident) error {
	if inc.ID == "" {
		inc.ID = util.NewID()
	}
	if inc.CreatedAt.IsZero() {
		inc.CreatedAt = time.Now().UTC()
	}
	if err := s.db.WithContext(ctx).Create(inc).Error; err != nil {
		return err
	}
	if s.m != nil && inc.Source != "" {
		s.m.Inc("aiops_incidents_total", inc.Source)
	}
	if s.notify != nil {
		s.notify(ctx, inc, "created")
	}
	return nil
}

// Get 查询 incident。
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Incident, error) {
	var inc Incident
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&inc).Error
	if err != nil {
		return nil, err
	}
	return &inc, nil
}

// List 查询 incident 列表。
func (s *Service) List(ctx context.Context, tenantID string, status string, limit, offset int) ([]Incident, int64, error) {
	stmt := s.db.WithContext(ctx).Model(&Incident{}).Where("tenant_id = ?", tenantID)
	if status != "" {
		stmt = stmt.Where("status = ?", status)
	}
	var total int64
	if err := stmt.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var items []Incident
	err := stmt.Order("created_at DESC").Limit(limit).Offset(offset).Find(&items).Error
	return items, total, err
}

// AttachAlert 幂等关联告警到 incident。
func (s *Service) AttachAlert(ctx context.Context, inc *Incident, a *alert.Alert) error {
	link := AlertToIncident{AlertID: a.ID, IncidentID: inc.ID, TenantID: inc.TenantID}
	// 先解除该告警在其它活跃 incident 的归属（一告警一 incident）
	_ = s.db.WithContext(ctx).Where("tenant_id = ? AND alert_id = ? AND incident_id <> ?",
		inc.TenantID, a.ID, inc.ID).Delete(&AlertToIncident{}).Error
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&link).Error
}

// AlertsOf 返回 incident 关联的告警。
func (s *Service) AlertsOf(ctx context.Context, tenantID, incidentID string) ([]alert.Alert, error) {
	var ids []string
	if err := s.db.WithContext(ctx).Model(&AlertToIncident{}).
		Where("tenant_id = ? AND incident_id = ?", tenantID, incidentID).
		Pluck("alert_id", &ids).Error; err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var items []alert.Alert
	err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&items).Error
	return items, err
}

// IncidentOfAlert 返回告警当前归属的 incident（如有）。
func (s *Service) IncidentOfAlert(ctx context.Context, tenantID, alertID string) (*Incident, error) {
	var link AlertToIncident
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND alert_id = ?", tenantID, alertID).First(&link).Error
	if err != nil {
		return nil, nil // 未关联
	}
	return s.Get(ctx, tenantID, link.IncidentID)
}

// Resolve 解决 incident，并触发 resolved 生命周期通知。
func (s *Service) Resolve(ctx context.Context, tenantID, id string) error {
	if err := s.resolveDB(ctx, tenantID, id); err != nil {
		return err
	}
	if s.notify != nil {
		inc, err := s.Get(ctx, tenantID, id)
		if err == nil {
			s.notify(ctx, inc, "resolved")
		}
	}
	return nil
}

// ResolveSilent 解决 incident 但不触发 resolved 通知。
// 编排器审批闭环专用：approve 已发送更丰富的「审批通过」通知，避免同一事件双条打扰。
func (s *Service) ResolveSilent(ctx context.Context, tenantID, id string) error {
	return s.resolveDB(ctx, tenantID, id)
}

// resolveDB 更新解决状态（resolved_at 幂等）。装配了索引钩子时同事务入队
// 知识索引任务（source_version=结案数据当前版本，无结案为 0：已解决事故即入知识库
// 作为 T1 参考，结案数据后续补录会生成新版本）。
func (s *Service) resolveDB(ctx context.Context, tenantID, id string) error {
	now := time.Now().UTC()
	if s.resHook == nil {
		return s.db.WithContext(ctx).Model(&Incident{}).
			Where("tenant_id = ? AND id = ?", tenantID, id).
			Updates(map[string]any{"status": IncidentResolved, "resolved_at": now}).Error
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Incident{}).
			Where("tenant_id = ? AND id = ?", tenantID, id).
			Updates(map[string]any{"status": IncidentResolved, "resolved_at": now}).Error; err != nil {
			return err
		}
		// 取结案数据当前版本（无结案数据视为 0）；查询失败不阻塞解决（reconciliation 兜底）
		var version int64
		_ = tx.Raw(`SELECT COALESCE(MAX(version), 0) FROM incident_resolutions
			WHERE tenant_id = ? AND incident_id = ?`, tenantID, id).Scan(&version).Error
		if err := s.resHook(ctx, tx, tenantID, id, version, "upsert"); err != nil {
			slog.Warn("incident: enqueue rag index job on resolve", "incident", id, "err", err)
		}
		return nil
	})
}

// UpdateLLMSummary 更新事故的 LLM 增强摘要（llmSummary 为空时不更新，保留现值）。
// 当前唯一写入方是 AI 聚类路径（Incident.LLMSummary 与确定性 Summary 并存）。
func (s *Service) UpdateLLMSummary(ctx context.Context, tenantID, id, llmSummary string) error {
	if llmSummary == "" {
		return nil
	}
	return s.db.WithContext(ctx).Model(&Incident{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).Update("llm_summary", llmSummary).Error
}

// UpdateNameSummary 更新名称/摘要（AI 或人工）。
func (s *Service) UpdateNameSummary(ctx context.Context, tenantID, id, name, summary string) error {
	updates := map[string]any{}
	if name != "" {
		updates["name"] = name
	}
	if summary != "" {
		updates["summary"] = summary
	}
	if len(updates) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Model(&Incident{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).Updates(updates).Error
}

// -------- 规则 CRUD（API 层调用）--------

// ListRules 返回某租户全部规则。
func (s *Service) ListRules(ctx context.Context, tenantID string) ([]Rule, error) {
	var items []Rule
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at DESC").Find(&items).Error
	return items, err
}

// CreateRule 创建规则（ID/Fingerprint 由调用方填充）。
func (s *Service) CreateRule(ctx context.Context, r *Rule) error {
	return s.db.WithContext(ctx).Create(r).Error
}

// CreateFromAgent 由 Agent 流水线创建 incident（source=agent），并绑定状态。
func (s *Service) CreateFromAgent(ctx context.Context, st *State, a *alert.Alert, name string) (*Incident, error) {
	inc := &Incident{
		ID:           st.ID, // incident 与 state 同 ID，天然绑定
		TenantID:     st.TenantID,
		Name:         name,
		Status:       IncidentOpen,
		Severity:     a.Severity,
		Source:       "agent",
		AgentStateID: &st.ID,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.Create(ctx, inc); err != nil {
		return nil, err
	}
	if err := s.AttachAlert(ctx, inc, a); err != nil {
		return nil, err
	}
	return inc, nil
}

// -------- 编排器状态（检查点）--------

// SaveState 保存/更新编排器状态（检查点）。
func (s *Service) SaveState(ctx context.Context, st *State) error {
	st.UpdatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"status", "alert_id", "node_results", "current_agent", "retry_count", "error_message", "updated_at"}),
	}).Create(st).Error
}

// GetState 按 incident_id 加载编排器状态。
func (s *Service) GetState(ctx context.Context, tenantID, id string) (*State, error) {
	var st State
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&st).Error
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// StateIncident 由状态关联的 incident（若有）。
func (s *Service) StateIncident(ctx context.Context, st *State) (*Incident, error) {
	return s.Get(ctx, st.TenantID, st.ID)
}

// marshalJSON 辅助（避免 datatypes.JSON 直接组装）。
func marshalJSON(v any) datatypes.JSON {
	b, _ := json.Marshal(v)
	return b
}
