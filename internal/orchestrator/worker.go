package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"

	"aiops/internal/alert"
	"aiops/internal/incident"
	"aiops/internal/pusher"
	"aiops/internal/queue"
)

// agentRunPayload pipeline 投递 agent.run 任务时的载荷。
type agentRunPayload struct {
	AlertID    string `json:"alert_id"`
	IncidentID string `json:"incident_id"`
}

// HandleAgentTask 处理 queue.agent.run 任务：装载状态 → 组装上下文 → 执行编排。
// 作为 queue.Handler 注册到队列消费循环。
func (o *Orchestrator) HandleAgentTask(ctx context.Context, t queue.Task) error {
	var p agentRunPayload
	if err := json.Unmarshal(t.Payload, &p); err != nil {
		return err
	}

	// 1. 加载告警（编排上下文的事实起点）
	a, err := o.alerts.Get(ctx, t.TenantID, p.AlertID)
	if err != nil {
		slog.Warn("orchestrator: alert not found for agent task", "alert", p.AlertID, "err", err)
		return err
	}

	// 2. 装载/创建 IncidentState（检查点）
	st, err := o.ensureState(ctx, t.TenantID, p.IncidentID, a)
	if err != nil {
		return err
	}

	// 3. 确保 incident 存在（规则路径可能已建，agent 路径需自建）
	inc, err := o.svc.StateIncident(ctx, st)
	if err != nil || inc == nil {
		inc, err = o.svc.CreateFromAgent(ctx, st, a, a.Name)
		if err != nil {
			return err
		}
		o.push.Broadcast(pusher.Event{Type: "incident.upsert", TenantID: t.TenantID, Data: inc})
	}

	// 4. 执行编排流水线
	return o.Run(ctx, st, a)
}

// ensureState 按 incident_id 加载现有状态，否则新建并落库。
func (o *Orchestrator) ensureState(ctx context.Context, tenantID, incidentID string, a *alert.Alert) (*incident.State, error) {
	if incidentID != "" {
		if st, err := o.svc.GetState(ctx, tenantID, incidentID); err == nil {
			return st, nil
		}
		// 有 incident 但尚未建状态：以 incident_id 为状态 ID 初始化
		st := incident.NewState(tenantID, a.ID)
		st.ID = incidentID
		if err := o.svc.SaveState(ctx, st); err != nil {
			return nil, err
		}
		return st, nil
	}
	// 无 incident：新建状态（ID 即后续 incident ID），incident 由 HandleAgentTask 创建
	st := incident.NewState(tenantID, a.ID)
	if err := o.svc.SaveState(ctx, st); err != nil {
		return nil, err
	}
	return st, nil
}
