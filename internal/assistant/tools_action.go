package assistant

import (
	"context"
	"errors"
	"time"

	"aiops/internal/incident"

	"gorm.io/gorm"
)


// tools_action.go：人工确认动作工具（AGENT_CHAT_PLAN §5.3 受控动作工具 / Phase 3）。
// 安全模型：模型只能「提出」动作请求（创建 pending AssistantActionRequest），
// 绝不在工具调用路径内执行任何副作用（§9.2 动作隔离）；创建由 runner.requestAction
// 完成（有 db），确认/拒绝由 Service.ConfirmAction/RejectAction 执行并重新校验。
// 工具注册表不依赖执行器——提出请求只需领域 Service 做「存在性预检」。

// registerActionTools 注册动作工具（依赖缺失不注册）。
func registerActionTools(r *Registry, deps ToolDeps) {
	if deps.Alerts != nil {
		registerAckAlert(r, deps)
	}
	if deps.Incidents != nil {
		registerResolveIncident(r, deps)
		registerApproveIncidentAction(r, deps)
	}
}

// errObjectNotFound 目标对象不存在（工具内部语义错误，降级为友好结果）。
var errObjectNotFound = errors.New("assistant: object not found")

// notFoundErr 领域 Get 错误归一化：alert/incident 仓储对不存在返回 gorm.ErrRecordNotFound
// （非 nil,nil 语义，与 Phase 2 get_similar_incidents 同坑）——统一映射为 errObjectNotFound。
func notFoundErr(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errObjectNotFound
	}
	return err
}

// registerAckAlert 确认告警（低风险：一次人工确认）。
func registerAckAlert(r *Registry, deps ToolDeps) {
	r.tools["ack_alert"] = Tool{
		Name: "ack_alert", Description: "确认（acknowledge）一条告警，表示已认领处理。需要人工确认后才会执行。",
		JSONSchema: schema(map[string]any{
			"alert_id": strProp("告警 ID（必填）"),
		}, "alert_id"),
		ReadOnly: false, Timeout: 3 * time.Second,
		Action: &ActionSpec{
			RiskLevel: RiskLow,
			Validate: func(ctx context.Context, in ToolCallCtx) error {
				a, err := deps.Alerts.Get(ctx, in.TenantID, stringArg(in.Args, "alert_id"))
				if err != nil {
					return notFoundErr(err)
				}
				if a == nil {
					return errObjectNotFound
				}
				return nil
			},
		},
	}
}

// registerResolveIncident 解决事故（中风险：改变事故状态并触发知识索引）。
func registerResolveIncident(r *Registry, deps ToolDeps) {
	r.tools["resolve_incident"] = Tool{
		Name: "resolve_incident", Description: "解决（关闭）一个事故。需要人工确认后才会执行；已解决的事故不可重复提出。",
		JSONSchema: schema(map[string]any{
			"incident_id": strProp("事故 ID（必填）"),
		}, "incident_id"),
		ReadOnly: false, Timeout: 3 * time.Second,
		Action: &ActionSpec{
			RiskLevel: RiskMedium,
			Validate: func(ctx context.Context, in ToolCallCtx) error {
				inc, err := deps.Incidents.Get(ctx, in.TenantID, stringArg(in.Args, "incident_id"))
				if err != nil {
					return notFoundErr(err)
				}
				if inc == nil {
					return errObjectNotFound
				}
				if inc.Status == incident.IncidentResolved {
					return errors.New("assistant: 事故已解决")
				}
				return nil
			},
		},
	}
}

// registerApproveIncidentAction 审批编排器自愈动作（高风险：复用 Orchestrator 审批语义）。
func registerApproveIncidentAction(r *Registry, deps ToolDeps) {
	r.tools["approve_incident_action"] = Tool{
		Name: "approve_incident_action", Description: "审批事故编排器挂起的自愈/变更动作（approve 或 reject）。需要人工确认后才会执行。",
		JSONSchema: schema(map[string]any{
			"incident_id": strProp("事故 ID（必填）"),
			"decision":    enumProp("审批决策", "approve", "reject"),
		}, "incident_id", "decision"),
		ReadOnly: false, Timeout: 3 * time.Second,
		Action: &ActionSpec{
			RiskLevel: RiskHigh,
			Validate: func(ctx context.Context, in ToolCallCtx) error {
				inc, err := deps.Incidents.Get(ctx, in.TenantID, stringArg(in.Args, "incident_id"))
				if err != nil {
					return notFoundErr(err)
				}
				if inc == nil {
					return errObjectNotFound
				}
				// 决策白名单（确认时 Orchestrator 会再次校验）
				if d := stringArg(in.Args, "decision"); d != "approve" && d != "reject" {
					return errors.New("assistant: 非法审批决策（应为 approve/reject）")
				}
				return nil
			},
		},
	}
}
