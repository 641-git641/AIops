package auth

import (
	"context"
	"time"

	"aiops/internal/util"

	"gorm.io/gorm"
)

// owner.go：owner key 引导（KEEPON 4.3 简单访问保护）。
// apikey 模式首次启动时生成 owner key 并仅打印一次明文（stdout，不经 slog 防落日志文件）；
// 已在 HTTP 监听前存在，彻底解决"apikeys 创建端点自身需要钥匙"的引导问题。

// EnsureOwnerKey 确保 default 租户存在 owner key：表空 → 创建并返回明文；已有任何 key → 返回空。
func (s *Service) EnsureOwnerKey(ctx context.Context) (string, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&APIKey{}).Where("tenant_id = ?", DefaultTenant).Count(&count).Error; err != nil {
		return "", err
	}
	if count > 0 {
		return "", nil
	}
	return s.createKey(ctx, DefaultTenant, "owner", true)
}

// RegenerateOwner 撤销旧 owner key 并生成新 owner（明文返回一次；事务保证不出现空窗）。
func (s *Service) RegenerateOwner(ctx context.Context, tenantID string) (string, error) {
	secret := util.NewID() + util.NewID()
	key := APIKey{
		ID: util.NewID(), TenantID: tenantID, Name: "owner",
		SecretHash: util.SHA256(secret), IsOwner: true, CreatedAt: time.Now().UTC(),
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tenant_id = ? AND is_owner = true", tenantID).Delete(&APIKey{}).Error; err != nil {
			return err
		}
		return tx.Create(&key).Error
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// createKey 建 key（明文仅此一次）。
func (s *Service) createKey(ctx context.Context, tenantID, name string, isOwner bool) (string, error) {
	secret := util.NewID() + util.NewID()
	key := APIKey{
		ID: util.NewID(), TenantID: tenantID, Name: name, IsOwner: isOwner,
		SecretHash: util.SHA256(secret), CreatedAt: time.Now().UTC(),
	}
	if err := s.db.WithContext(ctx).Create(&key).Error; err != nil {
		return "", err
	}
	return secret, nil
}
