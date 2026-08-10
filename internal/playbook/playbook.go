// Package playbook 实现自愈剧本表（数据驱动，可 UI 管理）。
// Playbook 表驱动自愈动作（level / command / blast_radius_factor / rollback），
// 爆炸半径超标强制降级 L2，默认 dry-run（安全优先）。
package playbook

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"aiops/internal/util"

	"gorm.io/gorm"
)

// 自愈级别。
const (
	LevelL0 = "L0" // 自动执行（低风险，且需 dry_run=false）
	LevelL1 = "L1" // oncall 确认
	LevelL2 = "L2" // tech-lead 人工审批（含爆炸半径超标强制降级）
)

// Playbook 自愈剧本。Name 与 RCA 的 suggestedActions 对齐（rollback/scale_up/...）。
type Playbook struct {
	ID                string    `gorm:"primaryKey;size:36" json:"id"`
	TenantID          string    `gorm:"size:64;uniqueIndex:uk_pb_tenant_name,priority:1" json:"tenantId"`
	Name              string    `gorm:"size:64;uniqueIndex:uk_pb_tenant_name,priority:2" json:"name"`
	Level             string    `gorm:"size:8" json:"level"`              // L0|L1|L2
	CommandTemplate   string    `gorm:"type:text" json:"commandTemplate"` // kubectl rollout undo deployment/{{.service}}
	BlastRadiusFactor float64   `json:"blastRadiusFactor"`                // 0~1，动作的固有爆炸半径
	RollbackCommand   string    `gorm:"type:text" json:"rollbackCommand"` // 回滚命令模板
	DryRunFirst       bool      `json:"dryRunFirst"`                      // 是否强制先 dry-run
	Enabled           bool      `json:"enabled"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// TableName 表名。
func (Playbook) TableName() string { return "playbooks" }

// IndexHook 知识索引钩子（RAG 装配层注入；nil 时不进事务，行为与现有完全一致）。
// 与 incident.IndexHook 同签名：同事务写 knowledge_jobs 保证索引最终一致。
type IndexHook func(ctx context.Context, tx *gorm.DB, tenantID, sourceID string, sourceVersion int64, op string) error

// Service 剧本仓储 + 默认种子。
type Service struct {
	db      *gorm.DB
	idxHook IndexHook
}

// NewService 创建剧本服务。
func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// SetIndexHook 注入知识索引钩子（RAG 装配层；nil 保持现有行为）。
func (s *Service) SetIndexHook(fn IndexHook) { s.idxHook = fn }

// Create 创建剧本（装配索引钩子时同事务入队 upsert）。
func (s *Service) Create(ctx context.Context, pb *Playbook) error {
	if pb.ID == "" {
		pb.ID = util.NewID()
	}
	now := time.Now().UTC()
	if pb.CreatedAt.IsZero() {
		pb.CreatedAt = now
	}
	pb.UpdatedAt = now
	if pb.Level == "" {
		pb.Level = LevelL1
	}
	if s.idxHook == nil {
		return s.db.WithContext(ctx).Create(pb).Error
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(pb).Error; err != nil {
			return err
		}
		if err := s.idxHook(ctx, tx, pb.TenantID, pb.ID, pb.UpdatedAt.UnixNano(), "upsert"); err != nil {
			slog.Warn("playbook: enqueue rag index job", "playbook", pb.ID, "err", err)
		}
		return nil
	})
}

// List 返回某租户全部剧本。
func (s *Service) List(ctx context.Context, tenantID string) ([]Playbook, error) {
	var items []Playbook
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).
		Order("blast_radius_factor ASC").Find(&items).Error
	return items, err
}

// Get 按名称查询剧本。
func (s *Service) Get(ctx context.Context, tenantID, name string) (*Playbook, error) {
	var pb Playbook
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND name = ?", tenantID, name).First(&pb).Error
	if err != nil {
		return nil, err
	}
	return &pb, nil
}

// Update 更新剧本（仅部分字段，安全字段除外）。
// 装配索引钩子时同事务入队：启用 → upsert（新版本=UpdatedAt），停用 → delete（撤销派生知识）。
func (s *Service) Update(ctx context.Context, pb *Playbook) error {
	pb.UpdatedAt = time.Now().UTC()
	updates := map[string]any{
		"level": pb.Level, "command_template": pb.CommandTemplate,
		"blast_radius_factor": pb.BlastRadiusFactor, "rollback_command": pb.RollbackCommand,
		"dry_run_first": pb.DryRunFirst, "enabled": pb.Enabled, "updated_at": pb.UpdatedAt,
	}
	if s.idxHook == nil {
		return s.db.WithContext(ctx).Model(&Playbook{}).
			Where("tenant_id = ? AND id = ?", pb.TenantID, pb.ID).Updates(updates).Error
	}
	op := "upsert"
	if !pb.Enabled {
		op = "delete"
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Playbook{}).
			Where("tenant_id = ? AND id = ?", pb.TenantID, pb.ID).Updates(updates).Error; err != nil {
			return err
		}
		if err := s.idxHook(ctx, tx, pb.TenantID, pb.ID, pb.UpdatedAt.UnixNano(), op); err != nil {
			slog.Warn("playbook: enqueue rag index job", "playbook", pb.ID, "err", err)
		}
		return nil
	})
}

// Delete 删除剧本（装配索引钩子时同事务入队 delete，撤销派生知识）。
func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	if s.idxHook == nil {
		return s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&Playbook{}).Error
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&Playbook{}).Error; err != nil {
			return err
		}
		if err := s.idxHook(ctx, tx, tenantID, id, 0, "delete"); err != nil {
			slog.Warn("playbook: enqueue rag delete job", "playbook", id, "err", err)
		}
		return nil
	})
}

// Resolve 按建议动作选择可用剧本：优先精确匹配 name，其次第一个可用的 L0-L2 剧本。
// 未找到时先补种默认剧本再重试一次（零配置演示）。
func (s *Service) Resolve(ctx context.Context, tenantID string, suggested []string) (*Playbook, error) {
	pb, err := s.firstEnabled(ctx, tenantID, suggested)
	if err == nil && pb != nil {
		return pb, nil
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err // 真实 DB 错误直接返回，不补种
	}
	// 无匹配或空表：补种默认剧本后重试
	if serr := s.EnsureDefaults(ctx, tenantID); serr != nil {
		return nil, serr
	}
	return s.firstEnabled(ctx, tenantID, suggested)
}

func (s *Service) firstEnabled(ctx context.Context, tenantID string, suggested []string) (*Playbook, error) {
	for _, name := range suggested {
		pb, err := s.Get(ctx, tenantID, name)
		if err != nil {
			continue
		}
		if pb.Enabled {
			return pb, nil
		}
	}
	// 全部不匹配：返回第一个可用剧本作为兜底
	var items []Playbook
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND enabled = ?", tenantID, true).
		Order("blast_radius_factor ASC").Limit(1).Find(&items).Error; err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &items[0], nil
}

// EnsureDefaults 若租户尚无任何剧本，补种内置默认剧本（幂等）。
func (s *Service) EnsureDefaults(ctx context.Context, tenantID string) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(&Playbook{}).
		Where("tenant_id = ?", tenantID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	for _, d := range defaults() {
		pb := d
		pb.ID = util.NewID()
		pb.TenantID = tenantID
		if err := s.Create(ctx, &pb); err != nil { // 走 Create：索引钩子/ID/时间戳统一处理
			return err
		}
	}
	return nil
}

// defaults 内置默认剧本（与 RCA suggestedActions 对齐）。
func defaults() []Playbook {
	now := time.Now().UTC()
	return []Playbook{
		{
			Name: "restart_pod", Level: LevelL0,
			CommandTemplate:   "kubectl rollout restart deployment/{{.service}}",
			RollbackCommand:   "kubectl rollout status deployment/{{.service}}",
			BlastRadiusFactor: 0.10, DryRunFirst: true, Enabled: true, CreatedAt: now, UpdatedAt: now,
		},
		{
			Name: "verify_dependency", Level: LevelL0,
			CommandTemplate:   "curl -sSf -m 5 http://{{.service}}/healthz",
			RollbackCommand:   "true",
			BlastRadiusFactor: 0.02, DryRunFirst: true, Enabled: true, CreatedAt: now, UpdatedAt: now,
		},
		{
			Name: "scale_up", Level: LevelL1,
			CommandTemplate:   "kubectl scale deployment/{{.service}} --replicas=+1",
			RollbackCommand:   "kubectl scale deployment/{{.service}} --replicas=-1",
			BlastRadiusFactor: 0.15, DryRunFirst: true, Enabled: true, CreatedAt: now, UpdatedAt: now,
		},
		{
			Name: "rollback", Level: LevelL1,
			CommandTemplate:   "kubectl rollout undo deployment/{{.service}}",
			RollbackCommand:   "kubectl rollout status deployment/{{.service}}",
			BlastRadiusFactor: 0.35, DryRunFirst: true, Enabled: true, CreatedAt: now, UpdatedAt: now,
		},
		{
			Name: "rate_limit", Level: LevelL2,
			CommandTemplate:   "curl -X POST -H 'Content-Type: application/json' -d '{\"enable\":true}' http://gateway/rate_limit/{{.service}}",
			RollbackCommand:   "curl -X POST -H 'Content-Type: application/json' -d '{\"enable\":false}' http://gateway/rate_limit/{{.service}}",
			BlastRadiusFactor: 0.05, DryRunFirst: true, Enabled: true, CreatedAt: now, UpdatedAt: now,
		},
	}
}
