package assistant

import (
	"context"
	"fmt"
	"strings"

	"aiops/internal/incident"
)

// context_builder.go：绑定对象事实摘要（AGENT_CHAT_PLAN §8.1 第 2 层）。
// thread 可绑定 incident 上下文（context_type/context_id）；run 开始时将绑定的
// 事故确定性事实注入 system（模型回答基于绑定事故，而非从零猜测）。
// 只含确定性事实（名称/状态/严重度/摘要/关联告警），不含 LLM 推测。

// ContextBuilder 绑定对象摘要构建器（thread 由 runner 加载后传入）。
type ContextBuilder struct {
	Incidents *incident.Service
}

// NewContextBuilder 创建构建器（incidents 必装配）。
func NewContextBuilder(incidents *incident.Service) *ContextBuilder {
	return &ContextBuilder{Incidents: incidents}
}

// Build 生成绑定对象事实摘要。无绑定或绑定对象不存在返回 ""（不报错——摘要失败
// 不阻塞 run；缺失对象由工具调用兜底给出错误结果）。
func (b *ContextBuilder) Build(ctx context.Context, tenantID string, th *AssistantThread) (string, error) {
	if b == nil || b.Incidents == nil || th == nil {
		return "", nil
	}
	if th.ContextType != "incident" || th.ContextID == "" {
		return "", nil
	}
	inc, err := b.Incidents.Get(ctx, tenantID, th.ContextID)
	if err != nil || inc == nil {
		return "", nil
	}
	alerts, err := b.Incidents.AlertsOf(ctx, tenantID, inc.ID)
	if err != nil {
		alerts = nil
	}
	names := make([]string, 0, len(alerts))
	for _, a := range alerts {
		names = append(names, a.Name)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("当前绑定事故 %s（%s/%s）：", firstNonEmpty(inc.AIGeneratedName, inc.Name), inc.Severity, inc.Status))
	if inc.Summary != "" {
		sb.WriteString("摘要：" + inc.Summary + "。")
	}
	if len(names) > 0 {
		sb.WriteString("关联告警：" + strings.Join(trimSlice(names, 10), "、") + "。")
	}
	sb.WriteString("回答应围绕该事故展开；需要细节时调用 get_incident / get_similar_incidents 工具。")
	return sb.String(), nil
}
