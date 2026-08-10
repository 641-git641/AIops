package assistant

import (
	"context"
	"encoding/json"
	"fmt"

	"aiops/internal/alert"
	"aiops/internal/incident"
	"aiops/internal/orchestrator"
)

// executor.go：动作执行器（Phase 3，AGENT_CHAT_PLAN §9.2 动作隔离）。
// 领域副作用只发生在确认路径（ConfirmAction → Execute），工具调用路径只创建请求。
// 执行器表驱动：toolName → 领域 Service 调用；所有动作复用现有领域校验与审计语义。

// ActionExecutor 动作执行器（确认时调用；未装配时 ConfirmAction 报错）。
type ActionExecutor struct {
	Alerts       *alert.Repository
	Incidents    *incident.Service
	Orchestrator *orchestrator.Orchestrator
}

// Execute 执行单个动作请求。返回 error 表示执行失败（请求置 failed，错误进 DecisionReason）。
func (e *ActionExecutor) Execute(ctx context.Context, tenantID string, req *AssistantActionRequest, actor, reason string) error {
	switch req.ToolName {
	case "ack_alert":
		return e.ackAlert(ctx, tenantID, req)
	case "resolve_incident":
		return e.resolveIncident(ctx, tenantID, req)
	case "approve_incident_action":
		return e.approveAction(ctx, tenantID, req, actor, reason)
	default:
		return fmt.Errorf("assistant: 未知动作工具 %s", req.ToolName)
	}
}

// ackAlert 确认告警（alert_id → status=acknowledged + last_alerts 同步）。
func (e *ActionExecutor) ackAlert(ctx context.Context, tenantID string, req *AssistantActionRequest) error {
	alertID := stringArg(argsOf(req.Arguments), "alert_id")
	if alertID == "" {
		return fmt.Errorf("assistant: ack_alert 缺 alert_id")
	}
	_, err := e.Alerts.UpdateStatus(ctx, tenantID, alertID, alert.StatusAcknowledged)
	return err
}

// resolveIncident 解决事故（复用现有 Resolve：状态更新 + 通知 + 知识索引入队）。
func (e *ActionExecutor) resolveIncident(ctx context.Context, tenantID string, req *AssistantActionRequest) error {
	incID := stringArg(argsOf(req.Arguments), "incident_id")
	if incID == "" {
		return fmt.Errorf("assistant: resolve_incident 缺 incident_id")
	}
	return e.Incidents.Resolve(ctx, tenantID, incID)
}

// approveAction 审批编排器动作（复用 ApproveAction：pending_approval 状态校验 + heal 闭环 + 审计）。
func (e *ActionExecutor) approveAction(ctx context.Context, tenantID string, req *AssistantActionRequest, actor, reason string) error {
	args := argsOf(req.Arguments)
	incID := stringArg(args, "incident_id")
	decision := stringArg(args, "decision")
	if incID == "" || (decision != "approve" && decision != "reject") {
		return fmt.Errorf("assistant: approve_incident_action 参数非法")
	}
	return e.Orchestrator.ApproveAction(ctx, tenantID, incID, decision, actor, reason)
}

// argsOf 请求参数快照 → map（参数在提出时已规范化，此处只读）。
func argsOf(raw []byte) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}
