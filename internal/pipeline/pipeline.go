// Package pipeline 实现统一事件管线（双轨架构的确定性热路径）。
// 参考 keep 的 process_event：归一化→维护窗口→去重→入库→规则关联→WS 推送→Agent 触发。
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"aiops/internal/alert"
	"aiops/internal/eventbus"
	"aiops/internal/incident"
	"aiops/internal/metrics"
	"aiops/internal/pusher"
	"aiops/internal/queue"
)

// ProcessResult 管线处理结果。
type ProcessResult struct {
	Kind       alert.DedupKind
	Incident   *incident.Incident
	Suppressed bool // 被维护窗口静默
}

// Pipeline 事件管线。所有 provider 的入站事件都汇聚到这里。
type Pipeline struct {
	dedup *Deduper
	repo  *alert.Repository
	rules *incident.RuleEngine
	svc   *incident.Service
	bus   eventbus.Bus
	push  pusher.Broadcaster
	q     queue.Queue
	// maintenance 维护窗口过滤（keep 式热路径静默；默认放行）。
	maintenance MaintenanceFilter
	// Agent 触发策略
	AgentTriggerSeverity string
	// AgentEnabled 是否投递 agent.run 任务（由装配层按配置注入，默认 true）。
	AgentEnabled bool
	// workflowTrigger 投递 workflow.trigger 任务（keep 式工作流引擎；由装配层注入，默认 nil 关闭）。
	workflowTrigger func(ctx context.Context, a *alert.Alert)
	// metrics 指标注册表（Phase 3 压测观测；nil 跳过埋点）。
	m *metrics.Registry
}

// New 创建管线。
func New(repo *alert.Repository, rules *incident.RuleEngine, svc *incident.Service,
	bus eventbus.Bus, push pusher.Broadcaster, q queue.Queue) *Pipeline {
	return &Pipeline{
		dedup: NewDeduper(repo),
		repo:  repo, rules: rules, svc: svc, bus: bus, push: push, q: q,
		maintenance:          NoopMaintenance{},
		AgentTriggerSeverity: alert.SeverityHigh, // severity>=high 进入 Agent 路径
		AgentEnabled:         true,
	}
}

// SetMaintenanceFilter 装配维护窗口过滤器（nil 则保持默认放行）。
func (p *Pipeline) SetMaintenanceFilter(m MaintenanceFilter) {
	if m != nil {
		p.maintenance = m
	}
}

// InvalidateMaintenance 维护窗口 CRUD 后使过滤缓存失效（仅 WindowFilter 生效）。
func (p *Pipeline) InvalidateMaintenance(tenantID string) {
	if f, ok := p.maintenance.(*WindowFilter); ok {
		f.Invalidate(tenantID)
	}
}

// SetWorkflowTrigger 装配工作流触发回调（nil 则关闭工作流引擎投递）。
func (p *Pipeline) SetWorkflowTrigger(fn func(ctx context.Context, a *alert.Alert)) {
	p.workflowTrigger = fn
}

// SetMetrics 装配指标注册表（Phase 3 压测观测；nil 关闭埋点）。
func (p *Pipeline) SetMetrics(m *metrics.Registry) { p.m = m }

// ProcessEvents 批量处理入站事件（webhook 可能一次带多条）。
func (p *Pipeline) ProcessEvents(ctx context.Context, events []*alert.IngestEvent) []*ProcessResult {
	results := make([]*ProcessResult, 0, len(events))
	for _, e := range events {
		if e == nil {
			continue
		}
		if r, err := p.ProcessEvent(ctx, e); err != nil {
			slog.Error("pipeline: process event", "provider", e.Provider, "name", e.Name, "err", err)
		} else if r != nil {
			results = append(results, r)
		}
	}
	return results
}

// ProcessEvent 单条事件处理（同步）。
func (p *Pipeline) ProcessEvent(ctx context.Context, e *alert.IngestEvent) (*ProcessResult, error) {
	// 指标：处理耗时直方图（defer 采集，覆盖全部返回路径）
	start := time.Now()
	defer func() {
		if p.m == nil {
			return
		}
		p.m.Observe("aiops_pipeline_duration_seconds", time.Since(start).Seconds())
	}()
	if e.Provider == "" || e.Name == "" {
		return nil, fmt.Errorf("pipeline: provider and name are required")
	}
	a := e.BuildAlert()

	// 1. 维护窗口过滤（keep 式热路径第 2 步）：命中 → 以 suppressed 入库，跳过规则/Incident/Agent
	suppressed, err := p.maintenance.IsSuppressed(ctx, a)
	if err != nil {
		slog.Warn("pipeline: maintenance filter", "alert", a.ID, "err", err)
		suppressed = false
	}

	// 2. 去重分类（热路径：只查 last_alerts 唯一索引；窗口期内 suppressed 同内容算重复）
	kind, err := p.dedup.Classify(ctx, a.TenantID, a.Fingerprint, a.AlertHash, suppressed)
	if err != nil {
		return nil, err
	}
	if kind == alert.DedupDuplicate {
		// 完全重复：广播去重事件（前端可感知），不入库不触发
		if p.m != nil {
			p.m.Inc("aiops_alerts_duplicated_total")
		}
		p.push.Broadcast(pusher.Event{Type: "alert.duplicate", TenantID: a.TenantID, Data: a.ToDTO()})
		return &ProcessResult{Kind: kind}, nil
	}

	if suppressed {
		a.Status = alert.StatusSuppressed
		if err := p.repo.SaveAlert(ctx, a, kind); err != nil {
			return nil, fmt.Errorf("pipeline: save suppressed alert: %w", err)
		}
		if p.m != nil {
			p.m.Inc("aiops_alerts_suppressed_total")
		}
		p.push.Broadcast(pusher.Event{Type: "alert.suppressed", TenantID: a.TenantID, Data: a.ToDTO()})
		p.publishEvent(ctx, "alert.suppressed", a, nil)
		return &ProcessResult{Kind: kind, Suppressed: true}, nil
	}

	// 3. 入库（事务：insert alerts + upsert last_alerts）
	if err := p.repo.SaveAlert(ctx, a, kind); err != nil {
		return nil, fmt.Errorf("pipeline: save alert: %w", err)
	}
	if p.m != nil {
		p.m.Inc("aiops_alerts_total", string(kind))
	}

	// 4. 规则引擎关联（确定性热路径）
	inc, _ := p.rules.Evaluate(ctx, a)

	// 5. WS 广播
	eventType := "alert.new"
	if kind == alert.DedupUpdate {
		eventType = "alert.update"
	}
	p.push.Broadcast(pusher.Event{Type: eventType, TenantID: a.TenantID, Data: a.ToDTO()})
	if inc != nil {
		p.push.Broadcast(pusher.Event{Type: "incident.upsert", TenantID: a.TenantID, Data: inc})
	}

	// 6. 事件总线（审计/扩散）
	p.publishEvent(ctx, eventType, a, inc)

	// 7. Agent 路径触发：severity>=high 或规则配置 runAgent（可全局关闭）
	if p.AgentEnabled && shouldRunAgent(a, inc) {
		p.triggerAgent(ctx, a, inc)
	}

	// 8. 工作流触发（keep 式：用户定义工作流监听告警事件；异步投递，由 workflow 引擎去重）
	if p.workflowTrigger != nil {
		p.workflowTrigger(ctx, a)
	}

	return &ProcessResult{Kind: kind, Incident: inc}, nil
}

// shouldRunAgent 判定是否进入 Agent 智能路径。
func shouldRunAgent(a *alert.Alert, inc *incident.Incident) bool {
	if alert.SeverityRank(a.Severity) >= alert.SeverityRank(alert.SeverityHigh) {
		return true
	}
	return inc != nil && inc.Source == "rule" // 规则命中也可进入（Phase 2 由 Rule.RunAgent 精确控制）
}

// triggerAgent 投递 agent.run 任务（异步，不阻塞 webhook 响应）。
func (p *Pipeline) triggerAgent(ctx context.Context, a *alert.Alert, inc *incident.Incident) {
	payload, _ := json.Marshal(map[string]any{
		"alert_id": a.ID,
		"incident_id": func() any {
			if inc != nil {
				return inc.ID
			}
			return nil
		}(),
	})
	task := queue.Task{Type: queue.TaskAgentRun, TenantID: a.TenantID, Payload: payload}
	if err := p.q.Enqueue(ctx, task); err != nil {
		slog.Warn("pipeline: enqueue agent.run", "alert", a.ID, "err", err)
	}
}

// publishEvent 发布到事件总线（审计/未来 Kafka 扩散）。
func (p *Pipeline) publishEvent(ctx context.Context, typ string, a *alert.Alert, inc *incident.Incident) {
	ev := &eventbus.Event{
		ID: a.ID, Topic: eventbus.TopicAlerts, Type: typ,
		TenantID: a.TenantID, Timestamp: time.Now().UnixMilli(),
		Payload: map[string]any{"alertId": a.ID, "fingerprint": a.Fingerprint, "severity": a.Severity},
	}
	if inc != nil {
		ev.IncidentID = inc.ID
	}
	_ = p.bus.Publish(ctx, ev)
}
