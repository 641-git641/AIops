package external

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"aiops/internal/util"

	"gorm.io/gorm"
)

// service.go：外部系统 CRUD + env 首启种子。

// SeedItem 环境变量种子条目（AIOPS_EXTERNALSYSTEMS JSON 数组元素）。
type SeedItem struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Type string `json:"type,omitempty"`
}

// Service 外部系统服务。
type Service struct {
	db *gorm.DB
}

// NewService 创建服务。
func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// Seed 首启种子：表为空时写入 env 配置（之后 DB 为准；已有数据不覆盖）。
// env 条目校验失败跳过并返回错误汇总（不阻断启动）。
func (s *Service) Seed(ctx context.Context, tenantID string, items []SeedItem) error {
	if len(items) == 0 {
		return nil
	}
	var n int64
	if err := s.db.WithContext(ctx).Model(&System{}).Where("tenant_id = ?", tenantID).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return nil // 已有数据（DB 为准）
	}
	var errs []string
	now := time.Now().UTC()
	for i, it := range items {
		if err := ValidateURL(it.URL); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", it.Name, err))
			continue
		}
		typ := it.Type
		if !ValidType(typ) {
			typ = TypeOther
		}
		sys := &System{
			ID: util.NewID(), TenantID: tenantID,
			Name: it.Name, URL: it.URL, Type: typ,
			Enabled: true, SortOrder: i,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.db.WithContext(ctx).Create(sys).Error; err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", it.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("external: 种子部分失败: %s", strings.Join(errs, "; "))
	}
	return nil
}

// List 全部条目（enabled 在前，sortOrder 升序）。
func (s *Service) List(ctx context.Context, tenantID string) ([]System, error) {
	var items []System
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).
		Order("enabled DESC, sort_order ASC, created_at ASC").Find(&items).Error
	return items, err
}

// Create 新建（URL/类型校验）。
func (s *Service) Create(ctx context.Context, tenantID string, in System) (*System, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, errors.New("external: 名称必填")
	}
	if err := ValidateURL(in.URL); err != nil {
		return nil, err
	}
	if !ValidType(in.Type) {
		in.Type = TypeOther
	}
	now := time.Now().UTC()
	sys := &System{
		ID: util.NewID(), TenantID: tenantID, Name: strings.TrimSpace(in.Name),
		URL: strings.TrimSpace(in.URL), Type: in.Type,
		Enabled:   in.Enabled,
		SortOrder: in.SortOrder,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(sys).Error; err != nil {
		return nil, err
	}
	return sys, nil
}

// Update 更新（不存在返回 nil,nil；URL 校验）。
func (s *Service) Update(ctx context.Context, tenantID, id string, in System) (*System, error) {
	sys, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if sys == nil {
		return nil, nil
	}
	if strings.TrimSpace(in.Name) != "" {
		sys.Name = strings.TrimSpace(in.Name)
	}
	if in.URL != "" {
		if err := ValidateURL(in.URL); err != nil {
			return nil, err
		}
		sys.URL = strings.TrimSpace(in.URL)
	}
	if ValidType(in.Type) {
		sys.Type = in.Type
	}
	sys.Enabled = in.Enabled
	sys.SortOrder = in.SortOrder
	sys.UpdatedAt = time.Now().UTC()
	if err := s.db.WithContext(ctx).Model(sys).Updates(map[string]any{
		"name": sys.Name, "url": sys.URL, "type": sys.Type,
		"enabled": sys.Enabled, "sort_order": sys.SortOrder, "updated_at": sys.UpdatedAt,
	}).Error; err != nil {
		return nil, err
	}
	return sys, nil
}

// Delete 删除。
func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).
		Delete(&System{}).Error
}

// Get 单条。
func (s *Service) Get(ctx context.Context, tenantID, id string) (*System, error) {
	var sys System
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&sys).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sys, nil
}

// ValidateURL 只允许 http/https（防 javascript: 等协议注入）。
func ValidateURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("external: URL 非法")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("external: 仅支持 http/https URL")
	}
	if u.Host == "" {
		return errors.New("external: URL 缺少主机名")
	}
	return nil
}
