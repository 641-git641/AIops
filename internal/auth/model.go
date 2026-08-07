// Package auth 实现 API key 认证与租户上下文。
package auth

import (
	"context"
	"time"

	"aiops/internal/util"

	"gorm.io/gorm"
)

// APIKey 租户 API key（仅存哈希，明文仅在创建时返回一次）。
type APIKey struct {
	ID         string     `gorm:"primaryKey;size:36" json:"id"`
	TenantID   string     `gorm:"size:64;index:idx_apikey_tenant" json:"tenantId"`
	Name       string     `gorm:"size:128" json:"name"`
	SecretHash string     `gorm:"size:64;uniqueIndex" json:"-"` // sha256(secret)
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// TableName 表名。
func (APIKey) TableName() string { return "api_keys" }

// Service 认证服务。
type Service struct {
	db *gorm.DB
}

// NewService 创建认证服务。
func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// Create 创建 API key，返回明文（仅此一次）。
func (s *Service) Create(ctx context.Context, tenantID, name string) (string, error) {
	secret := util.NewID() + util.NewID() // 两段 UUID 作明文密钥
	key := APIKey{
		ID: util.NewID(), TenantID: tenantID, Name: name,
		SecretHash: util.SHA256(secret), CreatedAt: time.Now().UTC(),
	}
	if err := s.db.WithContext(ctx).Create(&key).Error; err != nil {
		return "", err
	}
	return secret, nil
}

// Verify 校验明文 key，返回所属租户。
func (s *Service) Verify(ctx context.Context, secret string) (string, error) {
	var key APIKey
	err := s.db.WithContext(ctx).Where("secret_hash = ?", util.SHA256(secret)).First(&key).Error
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	s.db.WithContext(ctx).Model(&APIKey{}).Where("id = ?", key.ID).Update("last_used_at", now)
	return key.TenantID, nil
}
