// Package llm 通用大模型协议层（AGENT_CHAT_PLAN §5.1）：
// OpenAI 兼容 Chat Completions 的多轮 messages + 工具定义 + 流式 tool_calls 增量。
// internal/ai/llm.go 保留为 RCA/聚类兼容 facade，本包供对话式 Agent 使用（两路互不干扰）。
package llm

import "encoding/json"

// Role 消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 通用消息（任意角色数组；tool 消息携带 tool_call_id 回填工具结果）。
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// Tool 模型可见的工具定义（parameters 是 JSON Schema，原样透传）。
type Tool struct {
	Type     string       `json:"type"` // 恒 "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction 工具函数定义。
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema（注册表产出，原样透传）
}

// ToolCall 模型发起的一次工具调用。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // 恒 "function"
	Function ToolCallFunc `json:"function"`
}

// ToolCallFunc 工具调用函数（arguments 是 JSON 字符串，流式场景可能跨 chunk 碎片拼成）。
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Delta 单次流式增量（内容与工具调用碎片）。
type Delta struct {
	Content   string
	ToolCalls []DeltaToolCall
}

// DeltaToolCall 工具调用增量（同一 index 的 id/name/arguments 跨 chunk 拼接）。
type DeltaToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Usage token 用量（流式时仅最后 chunk 携带）。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// FinishReason 终止原因。
type FinishReason string

const (
	FinishToolCalls FinishReason = "tool_calls" // 模型请求调用工具（继续 loop）
	FinishStop      FinishReason = "stop"       // 最终回答
)

// Completion 一次模型调用的完整结果（流式拼接后）。
type Completion struct {
	Content      string
	ToolCalls    []ToolCall // 按 index 合并去重后的工具调用
	FinishReason FinishReason
	Usage        *Usage
}
