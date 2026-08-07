package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"text/template"
	"time"

	"aiops/internal/alert"
	"aiops/internal/incident"
	"aiops/internal/queue"
	"aiops/internal/util"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AgentRunner 由编排器实现：工作流 `provider: agent` 步骤将告警接入编排流水线。
// 以接口注入便于单测（不依赖 orchestrator 包）。
type AgentRunner interface {
	HandleAgentTask(ctx context.Context, t queue.Task) error
}

// Service 工作流服务：触发匹配 → 并发防重落库 → 异步执行 steps/actions。
type Service struct {
	db   *gorm.DB
	inc  *incident.Service
	orch AgentRunner
	http *http.Client
	// execSem 工作流执行并发上限：落库/防重在队列 worker 同步完成，执行异步搬进独立 goroutine
	// （有界），慢步骤（http/agent）不再阻塞后续 agent.run 等其他队列任务。
	execSem chan struct{}
	sched   intervalState // interval 触发器调度状态（进程内）
}

// NewService 创建工作流服务。orch 可为 nil（未装配时 agent 步骤报错，不崩溃）。
// execWorkers 为并发执行上限（<=0 默认 8）。
func NewService(db *gorm.DB, inc *incident.Service, orch AgentRunner, execWorkers int) *Service {
	if execWorkers <= 0 {
		execWorkers = 8
	}
	return &Service{
		db:      db,
		inc:     inc,
		orch:    orch,
		http:    &http.Client{Timeout: 5 * time.Second},
		execSem: make(chan struct{}, execWorkers),
		sched:   intervalState{last: map[string]time.Time{}},
	}
}

// triggerPayload pipeline 投递 workflow.trigger 任务时的载荷。
type triggerPayload struct {
	AlertID string `json:"alert_id"`
}

// HandleTriggerTask 处理 queue.workflow.trigger：加载告警并对命中的启用工作流执行一次（并发防重）。
func (s *Service) HandleTriggerTask(ctx context.Context, t queue.Task) error {
	var p triggerPayload
	if err := json.Unmarshal(t.Payload, &p); err != nil {
		return err
	}
	var a alert.Alert
	if err := s.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", p.AlertID, t.TenantID).First(&a).Error; err != nil {
		slog.Warn("workflow: alert not found", "alert", p.AlertID, "err", err)
		return nil // 告警可能已删除，静默跳过
	}
	return s.RunForAlert(ctx, t.TenantID, &a)
}

// RunForAlert 对告警执行所有命中的启用工作流（每条工作流经 dedup 只跑一次）。
func (s *Service) RunForAlert(ctx context.Context, tenantID string, a *alert.Alert) error {
	var wfs []Workflow
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND enabled = ?", tenantID, true).Find(&wfs).Error; err != nil {
		return err
	}
	for i := range wfs {
		wf := &wfs[i]
		def, err := Parse([]byte(wf.YAML))
		if err != nil {
			slog.Warn("workflow: parse", "workflow", wf.ID, "err", err)
			continue
		}
		if def.Workflow.Disabled || !matchesAny(def.Workflow.Triggers, a) {
			continue
		}
		if err := s.tryRun(ctx, tenantID, wf, def, a); err != nil {
			slog.Warn("workflow: run", "workflow", wf.ID, "alert", a.ID, "err", err)
		}
	}
	return nil
}

func matchesAny(triggers []Trigger, a *alert.Alert) bool {
	for _, tr := range triggers {
		if MatchesTrigger(tr, a) {
			return true
		}
	}
	return false
}

// tryRun 落库运行记录（唯一索引并发防重），插入成功（RowsAffected>0）才异步执行。
func (s *Service) tryRun(ctx context.Context, tenantID string, wf *Workflow, def *Def, a *alert.Alert) error {
	run := &WorkflowRun{
		ID:         util.NewID(),
		TenantID:   tenantID,
		WorkflowID: wf.ID,
		DedupKey:   DedupKey(wf.ID, a.Fingerprint),
		Trigger:    "alert",
		Status:     RunRunning,
		StartedAt:  time.Now().UTC(),
	}
	tx := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "workflow_id"}, {Name: "dedup_key"}},
		DoNothing: true,
	}).Create(run)
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 0 {
		slog.Debug("workflow: deduped (already ran)", "workflow", wf.ID, "fingerprint", a.Fingerprint)
		return nil // 并发/重复触发：已被唯一索引裁决，不做二次执行
	}
	s.launch(ctx, run, def, a)
	return nil
}

// launch 异步执行运行记录：受 execSem 并发上限约束，慢步骤不再占用队列 worker。
// 执行在独立 goroutine 中进行；panic/超时均落库为 failed（不拖垮进程）。
func (s *Service) launch(ctx context.Context, run *WorkflowRun, def *Def, a *alert.Alert) {
	select {
	case s.execSem <- struct{}{}:
	case <-ctx.Done():
		s.markRunFailed(run, "context canceled before execution")
		return
	}
	execCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	go func() {
		defer func() { <-s.execSem }()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("workflow: run panic", "run", run.ID, "panic", r)
				s.markRunFailed(run, fmt.Sprintf("panic: %v", r))
			}
		}()
		s.execute(execCtx, run, def, a)
	}()
}

// markRunFailed 将运行记录标记为 failed（异步启动前/panic 兜底）。
func (s *Service) markRunFailed(run *WorkflowRun, reason string) {
	run.Status = RunFailed
	run.Error = reason
	run.FinishedAt = time.Now().UTC()
	if err := s.db.WithContext(context.Background()).Model(&WorkflowRun{}).
		Where("id = ?", run.ID).Updates(map[string]any{
		"status": run.Status, "error": run.Error, "finished_at": run.FinishedAt,
	}).Error; err != nil {
		slog.Warn("workflow: mark run failed", "run", run.ID, "err", err)
	}
}

// RunManual 手动触发工作流执行（POST /workflows/:id/run）。可选 alertID 装载告警并按其 filters 匹配。
// 手动运行不做并发防重（dedup_key 每次唯一），便于反复演练。返回已落库的运行记录。
func (s *Service) RunManual(ctx context.Context, tenantID, wfID, alertID string) (*WorkflowRun, error) {
	wf, err := s.Get(ctx, tenantID, wfID)
	if err != nil {
		return nil, err
	}
	if !wf.Enabled {
		return nil, fmt.Errorf("workflow %s is disabled", wfID)
	}
	def, err := Parse([]byte(wf.YAML))
	if err != nil {
		return nil, err
	}
	if def.Workflow.Disabled {
		return nil, fmt.Errorf("workflow %s is disabled (yaml disabled: true)", wfID)
	}
	var a *alert.Alert
	if alertID != "" {
		var al alert.Alert
		if err := s.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", alertID, tenantID).First(&al).Error; err != nil {
			return nil, fmt.Errorf("workflow: alert %s not found: %w", alertID, err)
		}
		a = &al
	}
	run := &WorkflowRun{
		ID:         util.NewID(),
		TenantID:   tenantID,
		WorkflowID: wf.ID,
		DedupKey:   "manual:" + util.NewID(), // 每次手动运行唯一，不做防重
		Trigger:    "manual",
		Status:     RunRunning,
		StartedAt:  time.Now().UTC(),
	}
	if err := s.db.WithContext(ctx).Create(run).Error; err != nil {
		return nil, err
	}
	s.launch(ctx, run, def, a)
	return run, nil
}

// execute 顺序执行 steps + actions，结果写入运行记录。
func (s *Service) execute(ctx context.Context, run *WorkflowRun, def *Def, a *alert.Alert) {
	results, runErr := runSteps(def.Workflow.Steps, def.Workflow.Actions, a,
		func(step Step, al *alert.Alert) (map[string]any, error) {
			return s.runStep(ctx, run, step, al)
		})
	if runErr != nil {
		run.Status = RunFailed
		run.Error = runErr.Error()
	} else {
		run.Status = RunSucceeded
	}
	b, _ := json.Marshal(results)
	run.Result = datatypes.JSON(b)
	run.FinishedAt = time.Now().UTC()
	if uerr := s.db.WithContext(ctx).Model(&WorkflowRun{}).Where("id = ?", run.ID).Updates(map[string]any{
		"status": run.Status, "result": run.Result, "error": run.Error, "finished_at": run.FinishedAt,
	}).Error; uerr != nil {
		slog.Warn("workflow: update run", "run", run.ID, "err", uerr)
	}
}

// runSteps 顺序执行 steps 后 actions，任一步失败即停（fail-fast）。纯函数，便于单测。
func runSteps(steps, actions []Step, a *alert.Alert,
	dispatch func(Step, *alert.Alert) (map[string]any, error)) (map[string]any, error) {
	results := map[string]any{}
	all := make([]Step, 0, len(steps)+len(actions))
	all = append(all, steps...)
	all = append(all, actions...)
	for _, step := range all {
		res, err := dispatch(step, a)
		if err != nil {
			return results, fmt.Errorf("step %s: %w", step.Name, err)
		}
		results[step.Name] = res
	}
	return results, nil
}

// runStep 按 provider 类型分发执行。
func (s *Service) runStep(ctx context.Context, run *WorkflowRun, step Step, a *alert.Alert) (map[string]any, error) {
	switch step.Provider.Type {
	case "log":
		msg := renderTemplate(stringify(step.Provider.With["message"]), a)
		slog.Info("workflow: log step", "run", run.ID, "workflow", run.WorkflowID, "message", msg)
		return map[string]any{"message": msg}, nil
	case "http":
		return s.httpStep(ctx, step.Provider.With, a)
	case "agent":
		return s.agentStep(ctx, run, a)
	default:
		return nil, fmt.Errorf("unsupported provider type %q", step.Provider.Type)
	}
}

// httpStep 通用 webhook 动作：POST/PUT/GET 等 JSON 到 with.url。
// with 参数：url（必填）/ method（默认 POST）/ body（支持 {{.alert.*}} 模板）/ headers（map，值支持模板）/
// auth（{type: basic, username, password} 或 {type: bearer, token}，值支持模板）。
func (s *Service) httpStep(ctx context.Context, with map[string]any, a *alert.Alert) (map[string]any, error) {
	url := stringify(with["url"])
	if url == "" {
		return nil, fmt.Errorf("http step: missing with.url")
	}
	method := stringify(with["method"])
	if method == "" {
		method = http.MethodPost
	}
	body := renderTemplate(stringify(with["body"]), a)
	if body == "" {
		body = "{}"
	}
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if headers, ok := with["headers"].(map[string]any); ok {
		for k, v := range headers {
			req.Header.Set(k, renderTemplate(stringify(v), a))
		}
	}
	if auth, ok := with["auth"].(map[string]any); ok {
		switch stringify(auth["type"]) {
		case "basic":
			req.SetBasicAuth(renderTemplate(stringify(auth["username"]), a), renderTemplate(stringify(auth["password"]), a))
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+renderTemplate(stringify(auth["token"]), a))
		}
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return map[string]any{"status": resp.StatusCode, "body": string(respBody)}, nil
}

// agentStep 将告警接入 Agent 编排流水线（provider: agent）。
// 防重复：告警已有 open 的 agent incident（内置 severity>=high 路径已处理）→ 跳过；
// 已有 rule incident → 复用其状态继续编排；否则新建（HandleAgentTask 自建 incident）。
func (s *Service) agentStep(ctx context.Context, run *WorkflowRun, a *alert.Alert) (map[string]any, error) {
	if a == nil {
		return nil, fmt.Errorf("agent provider requires an alert (interval/manual 无告警运行不适用)")
	}
	if s.orch == nil {
		return nil, fmt.Errorf("agent provider: orchestrator not wired")
	}
	tenantID := run.TenantID
	inc, err := s.inc.IncidentOfAlert(ctx, tenantID, a.ID) // (nil, nil) 表示未关联
	if err != nil {
		slog.Warn("workflow: incident lookup", "alert", a.ID, "err", err) // DB 出错按未关联处理，但不应静默
	}
	if inc != nil && inc.Source == "agent" && inc.Status == incident.IncidentOpen {
		return map[string]any{"skipped": true, "reason": "already handled by agent path", "incidentId": inc.ID}, nil
	}
	incidentID := ""
	if inc != nil {
		incidentID = inc.ID // 规则已建 incident：复用其状态继续编排
	}
	payload, _ := json.Marshal(map[string]any{"alert_id": a.ID, "incident_id": incidentID})
	if err := s.orch.HandleAgentTask(ctx, queue.Task{Type: queue.TaskAgentRun, TenantID: tenantID, Payload: payload}); err != nil {
		return nil, err
	}
	out := map[string]any{"handled": true}
	if after, err := s.inc.IncidentOfAlert(ctx, tenantID, a.ID); err == nil && after != nil {
		out["incidentId"] = after.ID
		out["status"] = after.Status
	}
	return out, nil
}

// -------- CRUD（API 层调用）--------

// Create 创建工作流（先解析校验 YAML）。
func (s *Service) Create(ctx context.Context, wf *Workflow) error {
	if _, err := Parse([]byte(wf.YAML)); err != nil {
		return err
	}
	if wf.ID == "" {
		wf.ID = util.NewID()
	}
	now := time.Now().UTC()
	if wf.CreatedAt.IsZero() {
		wf.CreatedAt = now
	}
	wf.UpdatedAt = now
	if wf.Name == "" {
		wf.Name = wf.ID
	}
	return s.db.WithContext(ctx).Create(wf).Error
}

// List 返回某租户全部工作流。
func (s *Service) List(ctx context.Context, tenantID string) ([]Workflow, error) {
	var items []Workflow
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).
		Order("created_at ASC").Find(&items).Error
	return items, err
}

// Get 查询单个工作流。
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Workflow, error) {
	var wf Workflow
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&wf).Error
	if err != nil {
		return nil, err
	}
	return &wf, nil
}

// Update 更新工作流（yaml/enabled；yaml 非空时先校验）。
func (s *Service) Update(ctx context.Context, wf *Workflow) error {
	if wf.YAML != "" {
		if _, err := Parse([]byte(wf.YAML)); err != nil {
			return err
		}
	}
	wf.UpdatedAt = time.Now().UTC()
	updates := map[string]any{"enabled": wf.Enabled, "updated_at": wf.UpdatedAt}
	if wf.YAML != "" {
		updates["yaml"] = wf.YAML
	}
	return s.db.WithContext(ctx).Model(&Workflow{}).
		Where("tenant_id = ? AND id = ?", wf.TenantID, wf.ID).Updates(updates).Error
}

// Delete 删除工作流（关联运行记录保留，仅停用）。
func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&Workflow{}).Error
}

// ListRuns 分页返回运行记录（验收④：核对 dedup 后实际执行次数）。
func (s *Service) ListRuns(ctx context.Context, tenantID string, limit, offset int) ([]WorkflowRun, int64, error) {
	stmt := s.db.WithContext(ctx).Model(&WorkflowRun{}).Where("tenant_id = ?", tenantID)
	var total int64
	if err := stmt.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []WorkflowRun
	err := stmt.Order("started_at DESC").Limit(limit).Offset(offset).Find(&items).Error
	return items, total, err
}

// EnsureDefaults 若租户尚无工作流则补种内置演示工作流（零配置演示 provider: agent）。
func (s *Service) EnsureDefaults(ctx context.Context, tenantID string) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(&Workflow{}).Where("tenant_id = ?", tenantID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Create(&Workflow{
		ID:          util.NewID(),
		TenantID:    tenantID,
		Name:        "订单中危告警自动接入 Agent 编排",
		Description: "内置演示：medium 的 order 告警默认不进 Agent 路径，通过工作流显式接入编排器（monitor→rca→heal→change）",
		YAML:        defaultWorkflowYAML,
		Enabled:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error
}

// -------- 模板与工具 --------

// renderTemplate 渲染 {{.alert.*}} 模板（Go text/template，missingkey=zero；解析/执行失败回退原文）。
// a 为 nil 时（interval/manual 无告警运行）alert 上下文为空 map。
func renderTemplate(tmpl string, a *alert.Alert) string {
	if tmpl == "" {
		return ""
	}
	alertData := map[string]any{}
	if a != nil {
		labels := map[string]any{}
		_ = json.Unmarshal(a.Labels, &labels)
		alertData = map[string]any{
			"id": a.ID, "name": a.Name, "severity": a.Severity, "status": a.Status,
			"provider": a.Provider, "fingerprint": a.Fingerprint, "labels": labels,
		}
	}
	data := map[string]any{"alert": alertData}
	t, err := template.New("wf").Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return tmpl
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return tmpl
	}
	return buf.String()
}

func stringify(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}
