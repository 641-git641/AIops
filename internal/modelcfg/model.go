// Package modelcfg 大模型 API 接入管理（DB 持久化，仿 notifier 渠道管理模式）。
// 管理 Chat LLM 与 Assistant Chat（model_configs 表，env 的 AIOPS_LLM*/AIOPS_ASSISTANTCHAT*
// 仅作首启种子，之后 DB 为准）；Embedding（RAG 向量化）不在此管理——由 AIOPS_EMBEDDING* env
// 直接配置（见 internal/embedding）。
// 客户端在启动装配时构建 → 保存配置后需重启生效（不做热重载）。
// apiKey 只存明文于 DB，永不进入 API 响应（List/Get 以 apiKeySet 代替）。
package modelcfg

import (
	"errors"
	"fmt"
	"time"
)

// 固定用途键（一行一个用途，对齐"每个用途一个客户端"的架构）。
const (
	KeyChat          = "chat"          // Chat LLM：RCA 根因补充 + AI 聚类摘要
	KeyAssistantChat = "assistant_chat" // 对话 Agent 生成模型（AGENT_CHAT_PLAN §10.1）
	KindChat         = "chat"
	KindAssistantChat = "assistant_chat"
)

// ModelConfig 大模型 API 配置（model_configs 表）。
type ModelConfig struct {
	Key        string    `gorm:"primaryKey;size:32" json:"key"` // chat
	Kind       string    `gorm:"size:16" json:"kind"`
	Enabled    bool      `json:"enabled"`
	BaseURL    string    `gorm:"size:512" json:"baseURL"`
	APIKey     string    `gorm:"size:512" json:"-"` // 敏感：仅入库，永不序列化
	Model      string    `gorm:"size:128" json:"model"`
	TimeoutSec int       `json:"timeoutSec"`
	Stream     bool      `json:"stream"` // chat：SSE 流式
	UpdatedAt  time.Time `json:"updatedAt"`
}

// TableName 表名。
func (ModelConfig) TableName() string { return "model_configs" }

// ModelConfigDTO API 响应视图（apiKey 脱敏：只声明是否已配置）。
type ModelConfigDTO struct {
	Key        string     `json:"key"`
	Kind       string     `json:"kind"`
	Enabled    bool       `json:"enabled"`
	BaseURL    string     `json:"baseURL"`
	Model      string     `json:"model"`
	TimeoutSec int        `json:"timeoutSec"`
	Stream     bool       `json:"stream"`
	APIKeySet  bool       `json:"apiKeySet"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"` // 指针：缺行（未配置）时不输出零值
}

// ToDTO 脱敏视图（apiKey 永不出现）。
func ToDTO(m *ModelConfig) ModelConfigDTO {
	updated := m.UpdatedAt
	return ModelConfigDTO{
		Key: m.Key, Kind: m.Kind, Enabled: m.Enabled, BaseURL: m.BaseURL,
		Model: m.Model, TimeoutSec: m.TimeoutSec, Stream: m.Stream,
		APIKeySet: m.APIKey != "", UpdatedAt: &updated,
	}
}

// SaveInput 保存/测试请求体（apiKey 传空 = 保留旧值）。
type SaveInput struct {
	Enabled    bool   `json:"enabled"`
	BaseURL    string `json:"baseURL"`
	APIKey     string `json:"apiKey"`
	Model      string `json:"model"`
	TimeoutSec int    `json:"timeoutSec"`
	Stream     bool   `json:"stream"`
}

// TestResult 连通性测试结果。
type TestResult struct {
	Key   string `json:"key"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ValidateSave 校验保存输入（纯函数，可单测）：
// key 必须为 chat|assistant_chat；启用时要求 baseURL/model；未配置的 timeoutSec 回填默认值 15。
func ValidateSave(key string, in *SaveInput) error {
	if key != KeyChat && key != KeyAssistantChat {
		return fmt.Errorf("key 必须为 chat|assistant_chat，got %q", key)
	}
	if !in.Enabled {
		return nil // 停用不要求任何字段（停用=静默）
	}
	if in.BaseURL == "" || in.Model == "" {
		return errors.New("启用配置必须填写 baseURL 与 model")
	}
	if in.TimeoutSec <= 0 {
		in.TimeoutSec = 15
	}
	return nil
}
