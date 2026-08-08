// Package orchestrator 实现多 Agent 编排器状态机。
// WorkflowNode{Name, Agent, Condition, MaxRetries} 顺序流水线，
// 条件不满足自动跳过（fail-open），节点失败重试后不中断流程，每节点后检查点落库可恢复。
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"aiops/internal/agent"
	"aiops/internal/alert"
	"aiops/internal/eventbus"
	"aiops/internal/incident"
	"aiops/internal/notifier"
	"aiops/internal/pusher"
	"aiops/internal/util"

	"gorm.io/datatypes"
)

// Node 编排表节点。MVP 的编排表由装配层声明（如 monitor→rca→heal→change）。
type Node struct {
	Name       string
	Agent      agent.Agent
	Condition  func(*incident.State) bool // nil 恒执行；false 跳过
	MaxRetries int                        // 失败重试次数（之后 fail-open 继续）
}

// Orchestrator 编排器状态机：加载/保存 IncidentState 检查点，顺序驱动节点。
type Orchestrator struct {
	alerts *alert.Repository
	svc    *incident.Service
	nodes  []Node
	cfg    agent.Cfg
	bus    eventbus.Bus
	push   pusher.Broadcaster
	audit  *eventbus.AuditService // 审批/处置的持久化审计
	notify *notifier.Service      // 通知渠道（可为 nil，无渠道时静默跳过）
}

// New 创建编排器。
func New(alerts *alert.Repository, svc *incident.Service, nodes []Node,
	cfg agent.Cfg, bus eventbus.Bus, push pusher.Broadcaster, audit *eventbus.AuditService,
	notify *notifier.Service) *Orchestrator {
	return &Orchestrator{alerts: alerts, svc: svc, nodes: nodes, cfg: cfg,
		bus: bus, push: push, audit: audit, notify: notify}
}

// 审批决策。
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// ApproveAction 处理分级审批决策（approve/reject）。
// 审批是安全护栏链的最后一道：人工确认后动作才视为执行（MVP 模拟执行，仅落审计）。
// 拒绝时 incident 保持 open（需人工处置），编排器状态闭环。
func (o *Orchestrator) ApproveAction(ctx context.Context, tenantID, incidentID, decision, actor, reason string) error {
	if decision != DecisionApprove && decision != DecisionReject {
		return fmt.Errorf("非法审批决策：%q（应为 approve/reject）", decision)
	}
	st, err := o.svc.GetState(ctx, tenantID, incidentID)
	if err != nil {
		return fmt.Errorf("incident 无编排器状态：%w", err)
	}
	if st.Status != incident.StatePendingApproval {
		return fmt.Errorf("incident %s 当前状态 %q，无需审批", incidentID, st.Status)
	}

	// 1. 更新 heal 节点结果（approve→executed / reject→rejected）
	action := "unknown"
	if heal := st.NodeResult("heal"); heal != nil {
		if v, ok := heal["action"].(string); ok && v != "" {
			action = v
		}
		if decision == DecisionApprove {
			heal["status"] = "executed"
			heal["executedAt"] = time.Now().UTC().Format(time.RFC3339)
		} else {
			heal["status"] = "rejected"
		}
		st.SetNodeResult("heal", heal)
	}

	// 2. 编排器状态闭环
	st.Status = incident.StateResolved
	if err := o.svc.SaveState(ctx, st); err != nil {
		return err
	}

	// 3. incident 状态同步：approve 直接闭环；reject 保持 open + 备注人工跟进
	inc, err := o.svc.Get(ctx, tenantID, incidentID)
	if err != nil {
		return err
	}
	meta := marshalJSON(map[string]any{
		"action": action, "decision": decision, "actor": actor,
		"correlationId": st.CorrelationID,
	})
	if decision == DecisionApprove {
		// 审批闭环走 silent 解决：已发送「审批通过」通知，避免同一事件双条打扰
		if err := o.svc.ResolveSilent(ctx, tenantID, incidentID); err != nil {
			return err
		}
		// 执行 + 审批 两条审计 + 通知值班结果
		o.recordAudit(ctx, st, actor, "approval.granted", action, reason, meta)
		o.recordAudit(ctx, st, "auto-system", "heal.executed", action, "审批通过，动作标记为已执行（MVP 模拟）", meta)
		o.notifyAndAudit(ctx, st, actor, "approved", "approval.granted", reason)
	} else {
		// 拒绝：incident 保持 open，备注追加到摘要（保留既有内容）
		note := fmt.Sprintf("自愈动作 %s 被拒绝（%s），需人工处置", action, reason)
		summary := ""
		if inc != nil {
			summary = inc.Summary
			if summary != "" {
				summary += "\n"
			}
			summary += note
		}
		_ = o.svc.UpdateNameSummary(ctx, tenantID, incidentID, "", summary)
		o.recordAudit(ctx, st, actor, "approval.rejected", action, reason, meta)
		o.notifyAndAudit(ctx, st, actor, "rejected", "approval.rejected", reason)
	}

	// 4. WS + 事件广播
	inc, _ = o.svc.Get(ctx, tenantID, incidentID)
	if inc != nil {
		o.push.Broadcast(pusher.Event{Type: "incident.upsert", TenantID: tenantID, Data: inc})
	}
	o.push.Broadcast(pusher.Event{
		Type: "incident.state", TenantID: tenantID,
		Data: map[string]any{"incidentId": incidentID, "status": st.Status,
			"node": "approval", "outcome": decision, "nodeResults": st.NodeResults},
	})
	o.publishAgentEvent(ctx, st, "approval", "approval."+decision,
		map[string]any{"action": action, "actor": actor, "reason": reason})
	return nil
}

// notifyAndAudit 发送通知并落审计（通知失败仅记录，不阻断主流程）。
// action 为消息语义（pending_approval|approved|rejected）；target 为审计目标（与 approval.* 对齐）。
func (o *Orchestrator) notifyAndAudit(ctx context.Context, st *incident.State, actor, action, target, reason string) {
	if o.notify == nil {
		return
	}
	msg := o.buildNotifyMessage(st, action, actor)
	msg.IncidentID = st.ID
	msg.CorrelationID = st.CorrelationID
	results := o.notify.Notify(ctx, msg)
	if len(results) == 0 {
		o.recordAudit(ctx, st, actor, "notify.skipped", target, "未配置通知渠道（AIOPS_NOTIFIERCHANNELS 为空）", marshalJSON(map[string]any{"channels": o.notify.Enabled()}))
		return
	}
	auditAction := "notify.sent"
	if !o.notify.AllOK(results) {
		auditAction = "notify.failed"
	}
	o.recordAudit(ctx, st, actor, auditAction, target,
		fmt.Sprintf("通知 %s 结果：%v", action, results), marshalJSON(map[string]any{"channels": o.notify.Enabled(), "results": results}))
}

// buildNotifyMessage 从编排器状态组装通知消息（标题 = 告警名，正文 = 服务/动作/风险评分/状态）。
func (o *Orchestrator) buildNotifyMessage(st *incident.State, action, actor string) notifier.Message {
	monitor := st.NodeResult("monitor")
	heal := st.NodeResult("heal")
	change := st.NodeResult("change")

	title := "AIops 事故通知"
	severity := ""
	service := ""
	if monitor != nil {
		if v, _ := monitor["alertName"].(string); v != "" {
			title = "告警：" + v
		}
		if v, _ := monitor["severity"].(string); v != "" {
			severity = v
		}
		if v, _ := monitor["service"].(string); v != "" {
			service = v
		}
	}

	lines := make([]string, 0, 6)
	if service != "" {
		lines = append(lines, "**受影响服务**：`"+service+"`")
	}
	if severity != "" {
		lines = append(lines, "**严重度**：`"+severity+"`")
	}
	if heal != nil {
		act, _ := heal["action"].(string)
		level, _ := heal["level"].(string)
		br, _ := heal["blastRadius"].(float64)
		if act != "" {
			lines = append(lines, fmt.Sprintf("**自愈动作**：`%s`（级别 %s，爆炸半径 %.2f）", act, level, br))
		}
		if downgraded, _ := heal["downgraded"].(bool); downgraded {
			lines = append(lines, "**⚠️ 爆炸半径超限，动作已强制降级 L2**")
		}
	}
	if change != nil {
		score, _ := change["riskScore"].(float64)
		level, _ := change["level"].(string)
		decision, _ := change["decision"].(string)
		lines = append(lines, fmt.Sprintf("**变更风险**：%.2f（%s，%s）", score, level, decision))
	}
	switch action {
	case "pending_approval":
		lines = append(lines, "**状态**：⏳ 等待审批，请在平台处理")
	case "approved":
		lines = append(lines, "**状态**：✅ 审批通过，自愈动作已执行")
	case "rejected":
		lines = append(lines, "**状态**：⛔ 审批拒绝，需人工处置")
	}

	return notifier.Message{
		Title:    title,
		Content:  strings.Join(lines, "\n"),
		Severity: severity,
		Actor:    actor,
		Action:   action,
	}
}

// recordAudit 持久化审计记录（事件总线 audit topic 的 DB 落地）。
func (o *Orchestrator) recordAudit(ctx context.Context, st *incident.State, actor, action, target, reason string, meta datatypes.JSON) {
	if o.audit == nil {
		return
	}
	_, _ = o.audit.Record(ctx, eventbus.AuditLog{
		TenantID: st.TenantID, IncidentID: st.ID, CorrelationID: st.CorrelationID,
		Actor: actor, Action: action, Target: target, Reason: reason, Metadata: meta,
	})
}

// Run 顺序执行编排表（同步）。st 由调用方保证已落库（检查点基础）。
// 语义：条件路由跳过 → 每节点重试 ≤MaxRetries → 失败 fail-open → 每节点后 checkpoint。
func (o *Orchestrator) Run(ctx context.Context, st *incident.State, a *alert.Alert) error {
	prevStatus := st.Status // 通知去重：仅在本轮发生待审批转换时通知一次
	for _, n := range o.nodes {
		if n.Condition != nil && !n.Condition(st) {
			o.checkpoint(ctx, st, n.Name, "skipped")
			continue
		}

		st.CurrentAgent = n.Name
		st.RetryCount = 0
		c := &agent.Context{State: st, Alert: a, Cfg: o.cfg}

		var lastErr error
		for attempt := 0; attempt <= n.MaxRetries; attempt++ {
			st.RetryCount = attempt
			st.ErrorMessage = ""
			if err := n.Agent.Handle(ctx, c); err == nil {
				lastErr = nil
				break
			} else {
				lastErr = err
				st.ErrorMessage = err.Error()
				slog.Warn("orchestrator: agent failed", "node", n.Name,
					"attempt", attempt, "err", err)
			}
		}

		if lastErr != nil {
			// fail-open：节点最终失败不中断流水线，错误写入检查点
			st.ErrorMessage = lastErr.Error()
			o.checkpoint(ctx, st, n.Name, "failed")
			o.publishAgentEvent(ctx, st, n.Name, "agent.failed", map[string]any{"error": lastErr.Error()})
			continue
		}
		o.checkpoint(ctx, st, n.Name, "completed")
		o.publishAgentEvent(ctx, st, n.Name, "agent.completed", nil)
	}

	// 进入待审批状态时通知值班（审批是人机闭环的触发点，通知一次避免重跑重复打扰）
	if st.Status == incident.StatePendingApproval && prevStatus != incident.StatePendingApproval {
		o.notifyAndAudit(ctx, st, "auto-system", "pending_approval", "incident.pending_approval", "事故进入待审批，需人工决策")
	}
	return nil
}

// checkpoint 保存编排器状态（incident_states upsert）。
func (o *Orchestrator) checkpoint(ctx context.Context, st *incident.State, node, outcome string) {
	st.UpdatedAt = time.Now().UTC()
	if err := o.svc.SaveState(ctx, st); err != nil {
		slog.Error("orchestrator: checkpoint failed", "node", node, "state", st.ID, "err", err)
	}
	o.push.Broadcast(pusher.Event{
		Type: "incident.state", TenantID: st.TenantID,
		Data: map[string]any{"incidentId": st.ID, "currentAgent": st.CurrentAgent,
			"status": st.Status, "node": node, "outcome": outcome,
			"nodeResults": st.NodeResults},
	})
}

// marshalJSON 序列化任意值为 datatypes.JSON（审计 metadata 用）。
func marshalJSON(v any) datatypes.JSON {
	b, err := json.Marshal(v)
	if err != nil {
		return datatypes.JSON(`{}`)
	}
	return datatypes.JSON(b)
}

// publishAgentEvent 发布审计事件（correlation_id 贯穿全链路）。
func (o *Orchestrator) publishAgentEvent(ctx context.Context, st *incident.State, node, typ string, extra map[string]any) {
	payload := map[string]any{"node": node}
	for k, v := range extra {
		payload[k] = v
	}
	_ = o.bus.Publish(ctx, &eventbus.Event{
		ID:            util.NewID(),
		Topic:         eventbus.TopicAudit,
		Type:          typ,
		CorrelationID: st.CorrelationID,
		TenantID:      st.TenantID,
		IncidentID:    st.ID,
		Payload:       payload,
		Timestamp:     time.Now().UnixMilli(),
	})
}
