// Package agent 定义 Agent 抽象与上下文。参考 multi-agent-aiops：
// Agent 通过共享 IncidentState 接力，节点结果写入 NodeResults，条件路由由编排器判定。
package agent

import (
	"context"

	"aiops/internal/alert"
	"aiops/internal/incident"
	"aiops/internal/topology"
)

// Cfg Agent 运行配置。
type Cfg struct {
	DryRun        bool    // 自愈动作默认 dry-run（安全优先）
	RCAConfidence float64 // RCA 置信度门槛（heal 触发条件）
	MaxDepth      int     // 依赖链遍历深度
	// Heal 安全护栏
	BlastRadiusLimit float64 // 爆炸半径超限强制降级 L2（默认 0.2）
	AutoHealLevel    string  // 允许自动执行的最高级别（默认 "L0"，即仅 L0 自动）
}

// Context Agent 运行上下文（非 DB 模型，每次编排调用组装）。
type Context struct {
	State    *incident.State
	Alert    *alert.Alert
	Incident *incident.Incident // 可 nil（agent 首次触发时可能尚未建 incident）
	Topology *topology.Service
	Cfg      Cfg
}

// Agent 编排器中的节点处理单元。
// 约定：实现应在成功/失败时把结构化结果写入 ctx.State（SetNodeResult），
// 失败返回 error 由编排器重试（fail-open）。
type Agent interface {
	Name() string
	Handle(ctx context.Context, c *Context) error
}
