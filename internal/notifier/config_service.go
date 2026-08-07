package notifier

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ConfigService 通知渠道配置的 DB 持久化 + 热重载。
// env（AIOPS_NOTIFIERCHANNELS + webhook）仅作首启种子；之后 DB 为运行时事实源，
// UI 修改即时 Reload 生效，重启后不回退到 env。
type ConfigService struct {
	db  *gorm.DB
	svc *Service
}

// NewConfigService 创建渠道配置服务（svc 为当前生效的通知路由，Reload 就地重建）。
func NewConfigService(db *gorm.DB, svc *Service) *ConfigService {
	return &ConfigService{db: db, svc: svc}
}

// EnsureDefaults 首启种子：把 env 启用的渠道（AIOPS_NOTIFIERCHANNELS 逗号分隔 + 对应 webhook）
// 种入 DB（仅当该渠道尚无配置行时；已存在则保持 DB 现值，不回退 env）。
func (c *ConfigService) EnsureDefaults(ctx context.Context, channels string, webhooks map[string]string) error {
	for _, name := range strings.Split(channels, ",") {
		name = strings.TrimSpace(name)
		if !Supported(name) {
			continue
		}
		wh := webhooks[name]
		if wh == "" {
			continue
		}
		var cnt int64
		if err := c.db.WithContext(ctx).Model(&ChannelConfig{}).Where("name = ?", name).Count(&cnt).Error; err != nil {
			return err
		}
		if cnt > 0 {
			continue
		}
		cfg := ChannelConfig{Name: name, Enabled: true, Webhook: wh, UpdatedAt: time.Now().UTC()}
		if err := c.db.WithContext(ctx).Create(&cfg).Error; err != nil {
			return err
		}
	}
	return nil
}

// List 返回全部渠道配置（含未启用，UI 展示用）。
func (c *ConfigService) List(ctx context.Context) ([]ChannelConfig, error) {
	var items []ChannelConfig
	err := c.db.WithContext(ctx).Order("name").Find(&items).Error
	return items, err
}

// Get 返回单渠道配置。
func (c *ConfigService) Get(ctx context.Context, name string) (*ChannelConfig, error) {
	var cfg ChannelConfig
	err := c.db.WithContext(ctx).Where("name = ?", name).First(&cfg).Error
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Save 保存渠道配置（不存在则新建）并热重载通知路由。
func (c *ConfigService) Save(ctx context.Context, name string, enabled bool, webhook string) error {
	if !Supported(name) {
		return fmt.Errorf("不支持的渠道：%s（可选 dingtalk/feishu/wecom/http）", name)
	}
	cfg := ChannelConfig{Name: name, Enabled: enabled, Webhook: webhook, UpdatedAt: time.Now().UTC()}
	var cnt int64
	if err := c.db.WithContext(ctx).Model(&ChannelConfig{}).Where("name = ?", name).Count(&cnt).Error; err != nil {
		return err
	}
	var err error
	if cnt == 0 {
		err = c.db.WithContext(ctx).Create(&cfg).Error
	} else {
		err = c.db.WithContext(ctx).Model(&ChannelConfig{}).Where("name = ?", name).
			Updates(map[string]any{"enabled": enabled, "webhook": webhook, "updated_at": cfg.UpdatedAt}).Error
	}
	if err != nil {
		return err
	}
	return c.Reload(ctx)
}

// Reload 从 DB 重建通知路由（热更新，不换指针）。
func (c *ConfigService) Reload(ctx context.Context) error {
	items, err := c.List(ctx)
	if err != nil {
		return err
	}
	c.svc.Reload(items)
	return nil
}

// Test 向指定渠道发送测试消息（用 DB 配置，无需启用；便于 UI 验证新 webhook 后再保存）。
func (c *ConfigService) Test(ctx context.Context, name string) (Result, error) {
	if !Supported(name) {
		return Result{}, fmt.Errorf("不支持的渠道：%s", name)
	}
	cfg, err := c.Get(ctx, name)
	if err != nil {
		return Result{}, fmt.Errorf("渠道 %s 未配置：%w", name, err)
	}
	if cfg.Webhook == "" {
		return Result{}, fmt.Errorf("渠道 %s 未配置 webhook", name)
	}
	ch, err := BuildChannel(name, cfg.Webhook, c.svc.client)
	if err != nil {
		return Result{}, err
	}
	err = ch.Send(ctx, Message{
		Title: "AIops 渠道测试", Severity: "info",
		Content: "这是一条来自 AIops 通知渠道的测试消息。",
		Action:  "test",
	})
	r := Result{Channel: name, OK: err == nil}
	if err != nil {
		r.Error = err.Error()
	}
	return r, nil
}

// BuildChannel 按渠道名构造 Channel（通用 helper，供配置测试/其他装配复用）。
func BuildChannel(name, webhook string, client *http.Client) (Channel, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	switch name {
	case ChannelDingTalk:
		return NewDingTalk(webhook, client), nil
	case ChannelFeishu:
		return NewFeishu(webhook, client), nil
	case ChannelWecom:
		return NewWecom(webhook, client), nil
	case ChannelHTTP:
		return NewHTTP(webhook, client), nil
	}
	return nil, fmt.Errorf("不支持的渠道：%s", name)
}
