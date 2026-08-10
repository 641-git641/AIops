// Package pipeline 实现统一事件管线（双轨架构的确定性热路径）。
// 环节：归一化→维护窗口→去重→入库→规则关联→WS 推送→Agent 触发。
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
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
	// maintenance 维护窗口过滤（热路径静默；默认放行）。
	maintenance MaintenanceFilter
	// Agent 触发策略
	AgentTriggerSeverity string
	// AgentEnabled 是否投递 agent.run 任务（由装配层按配置注入，默认 true）。
	AgentEnabled bool
	// workflowTrigger 投递 workflow.trigger 任务（工作流引擎；由装配层注入，默认 nil 关闭）。
	workflowTrigger func(ctx context.Context, a *alert.Alert)
	// notifyTrigger 投递 notify.send 任务（通知节流；由装配层注入，默认 nil 关闭）。
	notifyTrigger func(ctx context.Context, a *alert.Alert)
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

// SetNotifyTrigger 装配告警级通知触发回调（KEEPON 5.4 节流；nil 关闭）。
func (p *Pipeline) SetNotifyTrigger(fn func(ctx context.Context, a *alert.Alert)) {
	p.notifyTrigger = fn
}

// SetMetrics 装配指标注册表（Phase 3 压测观测；nil 关闭埋点）。
func (p *Pipeline) SetMetrics(m *metrics.Registry) { p.m = m }

// ProcessEvents 批量处理入站事件（webhook 可能一次带多条）。
// 批量快路径（Phase 3g）：批内去重 1 次批量读 + 1 次单语句批量写（SaveAlerts），
// 业务副作用（规则/WS/Agent/工作流）逐事件执行；批量读失败自动降级逐事件路径。
func (p *Pipeline) ProcessEvents(ctx context.Context, events []*alert.IngestEvent) []*ProcessResult {
	if rs, ok := p.processEventsBatch(ctx, events); ok {
		return rs
	}
	slog.Warn("pipeline: batch read failed, fallback to sequential processing")
	return p.processEventsSequential(ctx, events)
}

// processEventsSequential 逐事件路径（Phase 3g 前语义；批量读失败降级用）。
func (p *Pipeline) processEventsSequential(ctx context.Context, events []*alert.IngestEvent) []*ProcessResult {
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

// batchWriteChunk 批量写分块：每块 ≤ 100×15 + 100×9 = 2400 参数，远低于 PG 参数上限。
const batchWriteChunk = 100

// processEventsBatch 批量快路径：组装 → 批量读 → 批内 overlay 分类 → 批量写 → 逐条副作用。
// 返回 (results, ok)；ok=false 仅当批量读失败（此时未写任何行，可安全降级逐事件）。
func (p *Pipeline) processEventsBatch(ctx context.Context, events []*alert.IngestEvent) ([]*ProcessResult, bool) {
	// Pass 1：组装（BuildAlert + 维护窗口 + 最终状态），纯内存
	type cand struct {
		a          *alert.Alert
		suppressed bool
		start      time.Time
	}
	var cands []cand
	tenants := make(map[string][]int) // 租户 → 事件在 cands 中的下标
	for _, e := range events {
		if e == nil {
			continue
		}
		if e.Provider == "" || e.Name == "" {
			slog.Error("pipeline: process event", "provider", e.Provider, "name", e.Name, "err", errInvalidEvent)
			continue
		}
		a := e.BuildAlert()
		suppressed, err := p.maintenance.IsSuppressed(ctx, a)
		if err != nil {
			slog.Warn("pipeline: maintenance filter", "alert", a.ID, "err", err)
			suppressed = false
		}
		if suppressed {
			a.Status = alert.StatusSuppressed
		}
		idx := len(cands)
		cands = append(cands, cand{a: a, suppressed: suppressed, start: time.Now()})
		tenants[a.TenantID] = append(tenants[a.TenantID], idx)
	}
	if len(cands) == 0 {
		return []*ProcessResult{}, true
	}

	// 批量读：逐租户一次读齐该批所有指纹的 last_alerts 状态（替代逐事件 GetLastAlert）
	base := make(map[batchKey]*alert.LastAlert, len(cands))
	for tenantID, idxs := range tenants {
		fps := make([]string, 0, len(idxs))
		seen := make(map[string]bool, len(idxs))
		for _, i := range idxs {
			fp := cands[i].a.Fingerprint
			if !seen[fp] {
				seen[fp] = true
				fps = append(fps, fp)
			}
		}
		rows, err := p.repo.GetLastAlerts(ctx, tenantID, fps)
		if err != nil {
			return nil, false
		}
		for fp, la := range rows {
			base[batchKey{tenantID, fp}] = la
		}
	}

	// 批内 overlay 分类（逐事件 kind；duplicate 立即广播，其余收集批量写）
	cs := make([]batchCandidate, len(cands))
	for i, c := range cands {
		cs[i] = batchCandidate{
			tenant:       c.a.TenantID,
			fp:           c.a.Fingerprint,
			alertHash:    c.a.AlertHash,
			finalStatus:  c.a.Status,
			windowActive: c.suppressed,
		}
	}
	kinds := classifyBatchLast(base, cs)
	resultsByIdx := make([]*ProcessResult, len(cands))
	var toWrite []*alert.Alert
	var writeKinds []alert.DedupKind
	var writeSrc []int // 对应 cands 下标
	for i, k := range kinds {
		if k == alert.DedupDuplicate {
			a := cands[i].a
			if p.m != nil {
				p.m.Inc("aiops_alerts_duplicated_total")
			}
			p.push.Broadcast(pusher.Event{Type: "alert.duplicate", TenantID: a.TenantID, Data: a.ToDTO()})
			resultsByIdx[i] = &ProcessResult{Kind: k}
			p.observeEvent(cands[i].start)
		} else {
			toWrite = append(toWrite, cands[i].a)
			writeKinds = append(writeKinds, k)
			writeSrc = append(writeSrc, i)
		}
	}

	// 批量写（单语句多行 CTE，分块；块失败逐条降级——单语句原子，失败未写任何行）
	for s := 0; s < len(toWrite); s += batchWriteChunk {
		e := min(s+batchWriteChunk, len(toWrite))
		chunk, chunkKinds, chunkSrc := toWrite[s:e], writeKinds[s:e], writeSrc[s:e]
		if err := p.repo.SaveAlerts(ctx, chunk, chunkKinds); err != nil {
			slog.Error("pipeline: batch save alerts", "n", len(chunk), "err", err)
			for j := range chunk {
				if err2 := p.repo.SaveAlert(ctx, chunk[j], chunkKinds[j]); err2 != nil {
					slog.Error("pipeline: save alert (batch fallback)", "alert", chunk[j].ID, "err", err2)
					p.observeEvent(cands[chunkSrc[j]].start)
					continue // 写失败事件不 finalize（结果缺位，同逐事件 SaveAlert 失败）
				}
				resultsByIdx[chunkSrc[j]] = p.finalizeSaved(ctx, chunk[j], chunkKinds[j], cands[chunkSrc[j]].suppressed)
				p.observeEvent(cands[chunkSrc[j]].start)
			}
			continue
		}
		for j := range chunk {
			resultsByIdx[chunkSrc[j]] = p.finalizeSaved(ctx, chunk[j], chunkKinds[j], cands[chunkSrc[j]].suppressed)
			p.observeEvent(cands[chunkSrc[j]].start)
		}
	}

	// 按原事件顺序归集结果（写失败事件缺位，与逐事件错误语义一致）
	results := make([]*ProcessResult, 0, len(cands))
	for _, r := range resultsByIdx {
		if r != nil {
			results = append(results, r)
		}
	}
	return results, true
}

// observeEvent 上报单事件处理耗时（批路径：Pass 1 起点 → 副作用完成；对齐 ProcessEvent 的 defer 采集）。
func (p *Pipeline) observeEvent(start time.Time) {
	if p.m != nil {
		p.m.Observe("aiops_pipeline_duration_seconds", time.Since(start).Seconds())
	}
}

// errInvalidEvent 非法入站事件（与 ProcessEvent 的校验错误同文案）。
var errInvalidEvent = errors.New("pipeline: provider and name are required")

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
		return nil, errInvalidEvent
	}
	a := e.BuildAlert()

	// 1. 维护窗口过滤（热路径第 2 步）：命中 → 以 suppressed 入库，跳过规则/Incident/Agent
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

	// 3. 入库（insert alerts + upsert last_alerts）+ 逐条副作用
	if suppressed {
		a.Status = alert.StatusSuppressed
	}
	if err := p.repo.SaveAlert(ctx, a, kind); err != nil {
		return nil, fmt.Errorf("pipeline: save alert: %w", err)
	}
	return p.finalizeSaved(ctx, a, kind, suppressed), nil
}

// finalizeSaved 告警已入库后的逐条副作用（规则/WS/总线/Agent/工作流；单事件与批量路径共用）。
func (p *Pipeline) finalizeSaved(ctx context.Context, a *alert.Alert, kind alert.DedupKind, suppressed bool) *ProcessResult {
	if suppressed {
		if p.m != nil {
			p.m.Inc("aiops_alerts_suppressed_total")
		}
		p.push.Broadcast(pusher.Event{Type: "alert.suppressed", TenantID: a.TenantID, Data: a.ToDTO()})
		p.publishEvent(ctx, "alert.suppressed", a, nil)
		return &ProcessResult{Kind: kind, Suppressed: true}
	}
	if p.m != nil {
		p.m.Inc("aiops_alerts_total", string(kind))
	}

	// 规则引擎关联（确定性热路径）
	inc, _ := p.rules.Evaluate(ctx, a)

	// WS 广播
	eventType := "alert.new"
	if kind == alert.DedupUpdate {
		eventType = "alert.update"
	}
	p.push.Broadcast(pusher.Event{Type: eventType, TenantID: a.TenantID, Data: a.ToDTO()})
	if inc != nil {
		p.push.Broadcast(pusher.Event{Type: "incident.upsert", TenantID: a.TenantID, Data: inc})
	}

	// 事件总线（审计/扩散）
	p.publishEvent(ctx, eventType, a, inc)

	// 告警级通知（KEEPON 5.4 节流；异步投递，热路径不阻塞；物理去重的 duplicate 不触发）
	if p.notifyTrigger != nil {
		p.notifyTrigger(ctx, a)
	}

	// Agent 路径触发：severity>=high 或规则配置 runAgent（可全局关闭）
	if p.AgentEnabled && shouldRunAgent(a, inc) {
		p.triggerAgent(ctx, a, inc)
	}

	// 工作流触发（用户定义工作流监听告警事件；异步投递，由 workflow 引擎去重）
	if p.workflowTrigger != nil {
		p.workflowTrigger(ctx, a)
	}
	return &ProcessResult{Kind: kind, Incident: inc}
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
