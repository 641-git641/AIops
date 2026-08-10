package modelcfg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"aiops/internal/ai"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// config_service.go：模型配置仓储 + 生效值解析（仿 notifier.ConfigService 模式）。
// Chat 生效语义：DB 行优先，无行回退 env；DB 读失败由调用方 fail-open 回退 env。
// Embedding 不走 DB：EffectiveEmbedding 为纯 env 映射（装配用）。

// ChatEnv env 层的 Chat 配置输入（main.go 从 *config.Config 组装，本包不依赖 config）。
type ChatEnv struct {
	Driver     string
	BaseURL    string
	APIKey     string
	Model      string
	TimeoutSec int
	Stream     bool
}

// EmbeddingEnv env 层的 Embedding 配置输入。
type EmbeddingEnv struct {
	Driver     string // off | openai-compat | local
	BaseURL    string
	APIKey     string
	Model      string
	Dimension  int
	TimeoutSec int
	BatchSize  int
}

// ChatSettings 生效的 Chat 配置（Source 标识来源，日志/排障用）。
type ChatSettings struct {
	Source     string // "db" | "env"
	Enabled    bool
	BaseURL    string
	APIKey     string
	Model      string
	TimeoutSec int
	Stream     bool
}

// EmbeddingSettings 生效的 Embedding 配置（仅 env 来源，无 DB 管理）。
type EmbeddingSettings struct {
	Driver     string // off | openai-compat | local
	Enabled    bool
	BaseURL    string
	APIKey     string
	Model      string
	Dimension  int
	TimeoutSec int
	BatchSize  int
}

// ConfigService 模型配置仓储。
type ConfigService struct {
	db *gorm.DB
}

// NewConfigService 创建服务。
func NewConfigService(db *gorm.DB) *ConfigService { return &ConfigService{db: db} }

// EnsureDefaults env 首启种子（仿 notifier.ConfigService.EnsureDefaults）：
// chat / assistant_chat 行仅当对应 env 已配 APIKey 时种入；已有行保持 DB 现值不回退 env。
// （embedding 不走 DB——由 AIOPS_EMBEDDING* env 直接配置，见 EffectiveEmbedding。）
func (c *ConfigService) EnsureDefaults(ctx context.Context, chat ChatEnv, asst ChatEnv) error {
	// 种子条件与生效门控对齐：需 driver=openai-compat 且 key 非空。
	for _, pair := range []struct {
		key  string
		kind string
		env  ChatEnv
	}{
		{KeyChat, KindChat, chat},
		{KeyAssistantChat, KindAssistantChat, asst},
	} {
		if pair.env.Driver == "openai-compat" && pair.env.APIKey != "" {
			if err := c.seedIfAbsent(ctx, ModelConfig{
				Key: pair.key, Kind: pair.kind, Enabled: true,
				BaseURL: pair.env.BaseURL, APIKey: pair.env.APIKey, Model: pair.env.Model,
				TimeoutSec: pair.env.TimeoutSec, Stream: pair.env.Stream, UpdatedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// seedIfAbsent 仅当该 key 尚无行时插入（并发首启幂等：唯一键冲突按已存在处理）。
func (c *ConfigService) seedIfAbsent(ctx context.Context, m ModelConfig) error {
	var count int64
	if err := c.db.WithContext(ctx).Model(&ModelConfig{}).Where("key = ?", m.Key).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	return c.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&m).Error
}

// List 固定返回 chat + assistant_chat 两行（缺行补零值行，前端免合并逻辑）；apiKey 脱敏。
func (c *ConfigService) List(ctx context.Context) ([]ModelConfigDTO, error) {
	var rows []ModelConfig
	if err := c.db.WithContext(ctx).Where("key IN ?", []string{KeyChat, KeyAssistantChat}).Order("key").Find(&rows).Error; err != nil {
		return nil, err
	}
	byKey := map[string]*ModelConfig{}
	for i := range rows {
		byKey[rows[i].Key] = &rows[i]
	}
	out := make([]ModelConfigDTO, 0, 2)
	for _, key := range []string{KeyChat, KeyAssistantChat} {
		if r := byKey[key]; r != nil {
			out = append(out, ToDTO(r))
		} else {
			out = append(out, ModelConfigDTO{Key: key, Kind: key, APIKeySet: false})
		}
	}
	return out, nil
}

// Get 查询单行（不存在返回 (nil, nil)）。
func (c *ConfigService) Get(ctx context.Context, key string) (*ModelConfig, error) {
	var m ModelConfig
	err := c.db.WithContext(ctx).Where("key = ?", key).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// Save 保存（新建或更新）；apiKey 传空 = 保留旧值（空白串按空处理）。返回脱敏 DTO。
func (c *ConfigService) Save(ctx context.Context, key string, in SaveInput) (*ModelConfigDTO, error) {
	in.APIKey = strings.TrimSpace(in.APIKey)
	if err := ValidateSave(key, &in); err != nil {
		return nil, err
	}
	old, err := c.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	m := ModelConfig{
		Key: key, Kind: key, Enabled: in.Enabled,
		BaseURL: in.BaseURL, Model: in.Model,
		TimeoutSec: in.TimeoutSec, Stream: in.Stream,
		UpdatedAt: time.Now().UTC(),
	}
	if in.APIKey != "" {
		m.APIKey = in.APIKey
	} else if old != nil {
		m.APIKey = old.APIKey // apiKey 传空 = 保留旧值
	}
	if old == nil {
		if err := c.db.WithContext(ctx).Create(&m).Error; err != nil {
			return nil, err
		}
	} else if err := c.db.WithContext(ctx).Model(&ModelConfig{}).Where("key = ?", key).
		Updates(map[string]any{
			"kind": m.Kind, "enabled": m.Enabled, "base_url": m.BaseURL,
			"api_key": m.APIKey, "model": m.Model,
			"timeout_sec": m.TimeoutSec, "stream": m.Stream, "updated_at": m.UpdatedAt,
		}).Error; err != nil {
		return nil, err
	}
	dto := ToDTO(&m)
	return &dto, nil
}

// Test 连通性测试：表单值优先、DB 兜底，构造客户端调一次端点。
// chat → POST {baseURL}/chat/completions（"ping"）。
// 安全：存量 apiKey 只允许发往 DB 已保存的 baseURL（表单换了主机则不带存量 key，防 key 外带）。
func (c *ConfigService) Test(ctx context.Context, key string, in SaveInput) (TestResult, error) {
	if key != KeyChat && key != KeyAssistantChat {
		return TestResult{Key: key, Error: fmt.Sprintf("key 必须为 chat|assistant_chat，got %q", key)}, nil
	}
	old, err := c.Get(ctx, key)
	if err != nil {
		return TestResult{Key: key, Error: err.Error()}, nil
	}
	// 表单值优先，缺省字段回退 DB 现值（空 body 时等价于测 DB 现值）
	if old != nil {
		if in.BaseURL == "" {
			in.BaseURL = old.BaseURL
		}
		if in.Model == "" {
			in.Model = old.Model
		}
		if in.APIKey == "" && in.BaseURL == old.BaseURL {
			in.APIKey = old.APIKey // 仅当目标主机与 DB 一致时才携带存量 key
		}
		if in.TimeoutSec <= 0 {
			in.TimeoutSec = old.TimeoutSec
		}
	}
	if in.TimeoutSec <= 0 {
		in.TimeoutSec = 15
	}
	client := ai.NewClient(in.BaseURL, in.APIKey, in.Model)
	client.SetTimeout(time.Duration(in.TimeoutSec) * time.Second)
	client.SetStream(false)
	if _, err := client.Complete(ctx, "ping", "ping"); err != nil {
		return TestResult{Key: key, OK: false, Error: err.Error()}, nil
	}
	return TestResult{Key: key, OK: true}, nil
}

// ResolveChat 装配用：DB 行优先，无行回退 env；DB 读失败返回 err（调用方 fail-open 回退 env）。
func (c *ConfigService) ResolveChat(ctx context.Context, env ChatEnv) (ChatSettings, error) {
	return c.resolve(ctx, KeyChat, env)
}

// ResolveAssistant 装配用：同 ResolveChat（assistant_chat key）。
func (c *ConfigService) ResolveAssistant(ctx context.Context, env ChatEnv) (ChatSettings, error) {
	return c.resolve(ctx, KeyAssistantChat, env)
}

// resolve 按 key 解析生效配置（两 key 共用同一逻辑）。
func (c *ConfigService) resolve(ctx context.Context, key string, env ChatEnv) (ChatSettings, error) {
	row, err := c.Get(ctx, key)
	if err != nil {
		return ChatSettings{}, err
	}
	return EffectiveChat(row, row != nil, env), nil
}

// EffectiveAssistant 生效值纯函数：语义与 EffectiveChat 相同（assistant_chat 行）。
func EffectiveAssistant(row *ModelConfig, found bool, env ChatEnv) ChatSettings {
	return EffectiveChat(row, found, env)
}

// EffectiveChat 生效值纯函数：有行 → 行值（Enabled 以行为准，停用=静默）；
// 无行 → 回退 env（Enabled 与现有 LLM 门控等价：driver=openai-compat 且 apiKey 非空）。
func EffectiveChat(row *ModelConfig, found bool, env ChatEnv) ChatSettings {
	if found && row != nil {
		return ChatSettings{
			Source: "db", Enabled: row.Enabled, BaseURL: row.BaseURL,
			APIKey: row.APIKey, Model: row.Model,
			TimeoutSec: row.TimeoutSec, Stream: row.Stream,
		}
	}
	return ChatSettings{
		Source: "env", Enabled: env.Driver == "openai-compat" && env.APIKey != "",
		BaseURL: env.BaseURL, APIKey: env.APIKey, Model: env.Model,
		TimeoutSec: env.TimeoutSec, Stream: env.Stream,
	}
}

// EffectiveEmbedding 生效值纯函数（embedding 不走 DB，仅 env 来源）：
// 透传 env driver（off/local/openai-compat，保留 mock 演示路径）；
// openai-compat 的 Enabled 需要 apiKey 非空（与 chat 门控对齐，防空 key 静默 401 降级）。
func EffectiveEmbedding(env EmbeddingEnv) EmbeddingSettings {
	return EmbeddingSettings{
		Driver: env.Driver,
		Enabled: env.Driver == "openai-compat" && env.APIKey != "",
		BaseURL: env.BaseURL, APIKey: env.APIKey, Model: env.Model,
		Dimension: env.Dimension, TimeoutSec: env.TimeoutSec, BatchSize: env.BatchSize,
	}
}
