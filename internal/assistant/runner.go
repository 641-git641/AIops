package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"aiops/internal/eventbus"
	"aiops/internal/llm"
	"aiops/internal/metrics"
	"aiops/internal/util"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// runner.go：Agent loop（AGENT_CHAT_PLAN §5.2）。
// 状态机：queued → running → model_call → (tool calls → execute → model_call)* → completed
// 硬限制：MaxModelRounds / MaxToolCalls / RunTimeout / ToolTimeout；超限、超时、模型失败
// 输出明确的降级回答，并保留已取得的确定性事实。
// 与告警热路径隔离：独立 DB 租约 worker + 信号量，不复用 queue.NewMemoryQueue。

// ErrModelNotConfigured 未配置生成模型（服务照常启动，run 快速失败）。
var ErrModelNotConfigured = errors.New("assistant: 生成模型未配置（assistant_chat）")

// ChatModel 生成模型接口（由 *llm.Client 实现；mock 先行时指向 mock 端点）。
type ChatModel interface {
	Chat(ctx context.Context, req llm.ChatRequest, onDelta func(llm.Delta)) (*llm.Completion, error)
}

// Runner 对话 Agent 运行器。
type Runner struct {
	db        *gorm.DB
	model     ChatModel // nil = 未配置（run 快速失败 model_not_configured）
	modelName string    // 指标标签（装配时传入配置模型名）
	reg       *Registry
	broker    *Broker
	audit     *eventbus.AuditService
	limits    Limits
	ctxBuilder *ContextBuilder // nil = 无绑定对象（Phase 2：thread context 摘要）
	m         *metrics.Registry
	sem       chan struct{} // 并发信号量（MaxConcurrency）
	workerID  string

	mu          sync.Mutex
	activeCount int
}

// NewRunner 创建运行器（model 可为 nil——未配置时 run 快速失败，服务照常启动；
// ctxBuilder 可为 nil——无绑定对象时跳过上下文摘要）。
func NewRunner(db *gorm.DB, model ChatModel, modelName string, reg *Registry,
	broker *Broker, audit *eventbus.AuditService, limits Limits, ctxBuilder *ContextBuilder) *Runner {
	return &Runner{
		db: db, model: model, modelName: modelName, reg: reg, broker: broker,
		audit: audit, limits: limits, ctxBuilder: ctxBuilder,
		sem:      make(chan struct{}, limits.MaxConcurrency),
		workerID: "assistant-" + util.NewID()[:8],
	}
}

// SetMetrics 注入指标注册表（可 nil——测试/未装配路径判空）。
func (r *Runner) SetMetrics(m *metrics.Registry) { r.m = m }

// ActiveRuns 当前执行中 run 数（metrics gauge 求值用）。
func (r *Runner) ActiveRuns() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeCount
}

// RunWorker 启动 run 领取 worker（对齐 rag RunIndexer 结构：ticker + 立即首轮 + ctx 取消优雅退出）。
func (r *Runner) RunWorker(ctx context.Context) {
	interval := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			slog.Info("assistant: runner stopped")
			return
		case <-time.After(interval):
			r.claimAndDispatch(ctx)
		}
	}
}

// claimAndDispatch 领取到期 run 并逐个执行（信号量限并发；领取后失败由租约复活重试）。
func (r *Runner) claimAndDispatch(ctx context.Context) {
	runs, err := ClaimRuns(ctx, r.db, r.workerID, "", maxClaimBatch)
	if err != nil {
		slog.Warn("assistant: claim runs", "err", err)
		return
	}
	for i := range runs {
		run := &runs[i]
		select {
		case r.sem <- struct{}{}:
			go func() {
				defer func() { <-r.sem }()
				r.mu.Lock()
				r.activeCount++
				r.mu.Unlock()
				defer func() {
					r.mu.Lock()
					r.activeCount--
					r.mu.Unlock()
				}()
				r.processClaimed(ctx, run)
			}()
		default:
			// 信号量满：任务留待下轮领取（租约未动，不重复领取）
			slog.Warn("assistant: concurrency limit, run stays queued", "run", run.ID)
		}
	}
}

// ProcessClaimed 手动结算一次已领取的 run（测试与运维重跑入口；worker 内部走 processClaimed）。
func (r *Runner) ProcessClaimed(ctx context.Context, run *AssistantRun) { r.processClaimed(ctx, run) }

// processClaimed 结算一次领取的 run（对齐 rag processClaimed：成功 Complete / 失败 FailRun）。
func (r *Runner) processClaimed(ctx context.Context, run *AssistantRun) {
	start := time.Now()
	_, _ = r.broker.Publish(ctx, run.ID, run.TenantID, EvRunStarted, map[string]any{
		"status": RunRunning, "model": run.Model,
	})
	err := r.executeLoop(ctx, run)
	latency := time.Since(start)
	if err == nil {
		if cerr := CompleteRun(ctx, r.db, run.ID, latency.Milliseconds()); cerr != nil {
			slog.Error("assistant: complete run", "run", run.ID, "err", cerr)
		}
		_, _ = r.broker.Publish(ctx, run.ID, run.TenantID, EvRunCompleted, map[string]any{"latencyMs": latency.Milliseconds()})
		r.incRuns(RunCompleted)
		r.observeRun(latency.Seconds())
		return
	}
	// 取消不算失败
	var cancelled bool
	if get, _ := GetRun(ctx, r.db, run.TenantID, run.ID); get != nil && get.Status == RunCancelled {
		cancelled = true
	}
	if cancelled {
		_, _ = r.broker.Publish(ctx, run.ID, run.TenantID, EvRunCancelled, map[string]any{})
		r.incRuns(RunCancelled)
		return
	}
	if ferr := FailRun(ctx, r.db, run.ID, run.Attempts, err); ferr != nil {
		slog.Error("assistant: fail run", "run", run.ID, "err", ferr)
	}
	_, _ = r.broker.Publish(ctx, run.ID, run.TenantID, EvRunFailed, map[string]any{
		"error": truncateStr(err.Error(), 300),
	})
	r.incRuns(RunFailed)
	r.observeRun(latency.Seconds())
}

// executeLoop 执行 Agent loop。返回 nil 表示完成；error 表示失败（调用方 FailRun）。
func (r *Runner) executeLoop(ctx context.Context, run *AssistantRun) error {
	if r.model == nil {
		return ErrModelNotConfigured
	}
	// 阶段超时（整体受 RunTimeout 约束；取消传播到模型调用）
	loopCtx, cancel := context.WithTimeout(ctx, r.limits.RunTimeout)
	defer cancel()

	history, err := ListMessages(loopCtx, r.db, run.TenantID, run.ThreadID)
	if err != nil {
		return err
	}

	// 绑定对象事实摘要（AGENT_CHAT_PLAN §8.1 第 2 层；每次 run 重新解析，避免陈旧）
	contextSummary := ""
	if r.ctxBuilder != nil {
		if th, _ := GetThread(loopCtx, r.db, run.TenantID, run.ThreadID); th != nil {
			contextSummary, _ = r.ctxBuilder.Build(loopCtx, run.TenantID, th)
		}
	}

	// 本轮 run 命中的证据引用（工具调用累计 → 最终 assistant 消息引用卡）
	citations := []Citation{}

	var toolRound []llm.Message
	toolCalls := 0
	round := 0
	ordinal := 0

	for round < r.limits.MaxModelRounds {
		// 取消检查（cancel 端点改了 DB 状态；执行中的 run 每轮自查）
		if cur, _ := GetRun(loopCtx, r.db, run.TenantID, run.ID); cur != nil && cur.Status == RunCancelled {
			return nil // processClaimed 按 cancelled 分支结算
		}
		msgs, _ := BuildMessages(SystemPrompt(run.PromptVersion), contextSummary, history, toolRound)
		req := llm.ChatRequest{
			Messages: msgs,
			Tools:    r.reg.ToLLMTools(),
			Temperature: 0.3,
		}
		comp, err := r.callModel(loopCtx, run, req)
		if err != nil {
			return err
		}
		round++

		if len(comp.ToolCalls) == 0 {
			// 最终回答（无工具调用 → 结束 loop）
			content := comp.Content
			if content == "" {
				content = "（模型未返回内容）"
			}
			seq, err := NextMessageSequence(loopCtx, r.db, run.TenantID, run.ThreadID)
			if err != nil {
				return err
			}
			msg := &AssistantMessage{
				ID: util.NewID(), TenantID: run.TenantID, ThreadID: run.ThreadID, RunID: run.ID,
				Role: MsgAssistant, Content: content, ContentFormat: "text",
				Sequence: seq, Status: "complete",
			}
			if len(citations) > 0 {
				msg.Citations = datatypes.JSON(jsonOf(citations)) // 引用卡数据源
			}
			if err := CreateMessage(loopCtx, r.db, msg); err != nil {
				return err
			}
			_, _ = r.broker.Publish(loopCtx, run.ID, run.TenantID, EvMessageDone, map[string]any{
				"messageId": msg.ID, "content": content,
				"citations": citations,
			})
			return nil
		}

		// 工具轮：串行执行（未知/参数非法 → 拒绝并继续，不终止 run）
		assistantMsg := llm.Message{Role: llm.RoleAssistant, Content: "", ToolCalls: comp.ToolCalls}
		toolRound = append(toolRound, assistantMsg)
		for _, tc := range comp.ToolCalls {
			if toolCalls >= r.limits.MaxToolCalls {
				return errors.New("assistant: tool call budget exceeded")
			}
			toolCalls++
			ordinal++
			result, _ := r.executeTool(loopCtx, run, tc, ordinal, &citations) // 工具失败已降级为错误结果文本，不中断 loop
			toolRound = append(toolRound, buildToolMessage(tc.ID, tc.Function.Name, result))
		}
	}
	return errors.New("assistant: model round budget exceeded")
}

// callModel 调模型并流式转发 message.delta（增量发布到 broker）。
func (r *Runner) callModel(ctx context.Context, run *AssistantRun, req llm.ChatRequest) (*llm.Completion, error) {
	start := time.Now()
	var sb []string
	comp, err := r.model.Chat(ctx, req, func(d llm.Delta) {
		if d.Content != "" {
			sb = append(sb, d.Content)
			_, _ = r.broker.Publish(ctx, run.ID, run.TenantID, EvMessageDelta, map[string]any{
				"content": d.Content,
			})
		}
	})
	if err != nil {
		r.incModel("failed", time.Since(start).Seconds())
		return nil, err
	}
	r.incModel("ok", time.Since(start).Seconds())
	if comp.Usage != nil {
		_, _ = r.broker.Publish(ctx, run.ID, run.TenantID, EvUsageUpdated, map[string]any{
			"totalTokens": comp.Usage.TotalTokens,
		})
	}
	return comp, nil
}

// executeTool 校验并执行单个工具调用。返回回喂模型的工具结果文本。
// citations 收集器：工具命中证据时追加；追加部分发布 citation.added 事件（引用卡实时展示）。
func (r *Runner) executeTool(ctx context.Context, run *AssistantRun, tc llm.ToolCall, ordinal int, citations *[]Citation) (string, error) {
	tool, ok := r.reg.Get(tc.Function.Name)
	if !ok {
		r.recordToolStep(ctx, run, ordinal, tc, StepToolCall, "rejected", "unknown tool", 0, "")
		r.incTool(tc.Function.Name, "unknown", 0)
		return `{"error":"未知工具 ` + tc.Function.Name + `"}`, nil
	}
	// 参数解析 + schema 校验（模型生成的参数不可信）
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		r.recordToolStep(ctx, run, ordinal, tc, StepToolCall, "rejected", "invalid json args", 0, "")
		r.incTool(tool.Name, "rejected", 0)
		return `{"error":"工具参数不是合法 JSON"}`, nil
	}
	if err := validateToolArgs(tool.JSONSchema, args); err != nil {
		r.recordToolStep(ctx, run, ordinal, tc, StepPolicy, "rejected", err.Error(), 0, "")
		r.incTool(tool.Name, "rejected", 0)
		return `{"error":"` + truncateStr(err.Error(), 200) + `"}`, nil
	}

	_, _ = r.broker.Publish(ctx, run.ID, run.TenantID, EvToolStarted, map[string]any{
		"tool": tool.Name, "args": joinArgs(args),
	})
	start := time.Now()
	before := 0
	if citations != nil {
		before = len(*citations)
	}
	toolCtx, cancel := context.WithTimeout(ctx, r.limits.ToolTimeout)
	defer cancel()
	out, err := tool.Call(toolCtx, ToolCallCtx{
		TenantID: run.TenantID, RunID: run.ID, Args: args, Citations: citations,
	})
	dur := time.Since(start).Seconds()
	result := sprintJSON(out)
	if err != nil {
		r.recordToolStep(ctx, run, ordinal, tc, StepToolRes, "failed", err.Error(), dur, "")
		_, _ = r.broker.Publish(ctx, run.ID, run.TenantID, EvToolFailed, map[string]any{
			"tool": tool.Name, "error": truncateStr(err.Error(), 200),
		})
		r.incTool(tool.Name, "failed", dur)
		return `{"error":"` + truncateStr(err.Error(), 200) + `"}`, nil
	}
	// 新增引用逐条发布（引用卡实时出现；按 (runId,sequence) 幂等去重）
	if citations != nil && len(*citations) > before {
		for _, c := range (*citations)[before:] {
			_, _ = r.broker.Publish(ctx, run.ID, run.TenantID, EvCitationAdded, map[string]any{
				"citation": c,
			})
			r.incCitations(c.SourceType)
		}
	}
	result = trimToolResult(result)
	r.recordToolStep(ctx, run, ordinal, tc, StepToolRes, "ok", "", dur, result)
	_, _ = r.broker.Publish(ctx, run.ID, run.TenantID, EvToolCompleted, map[string]any{
		"tool": tool.Name, "resultSummary": truncateStr(result, 500),
	})
	r.incTool(tool.Name, "ok", dur)
	r.auditTool(ctx, run, tool.Name, args)
	return result, nil
}

// recordToolStep 落库工具步骤（arguments/result 只存裁剪摘要；result 为空时按错误构造）。
func (r *Runner) recordToolStep(ctx context.Context, run *AssistantRun, ordinal int, tc llm.ToolCall,
	kind, status, errMsg string, dur float64, result string) {
	step := &AssistantStep{
		ID: util.NewID(), TenantID: run.TenantID, RunID: run.ID, Ordinal: ordinal, Kind: kind,
		ToolName:   tc.Function.Name,
		Arguments:  datatypes.JSON(truncateStr(tc.Function.Arguments, 2000)),
		Status:     status,
		DurationMS: int64(dur * 1000),
		ErrorCode:  truncateStr(errMsg, 300),
	}
	if result != "" {
		step.ResultSummary = datatypes.JSON(trimToolResult(result))
	} else if errMsg != "" {
		step.ResultSummary = datatypes.JSON(`{"error":"` + truncateStr(errMsg, 200) + `"}`)
	}
	_ = InsertStep(ctx, r.db, step)
}

// auditTool 工具调用审计（CorrelationID=runID；工具结果标记 untrusted 不写审计）。
func (r *Runner) auditTool(ctx context.Context, run *AssistantRun, toolName string, args map[string]any) {
	if r.audit == nil {
		return
	}
	_, _ = r.audit.Record(ctx, eventbus.AuditLog{
		TenantID: run.TenantID, CorrelationID: run.ID,
		Actor:  "local-noauth", // 本期无真实身份（AGENT_CHAT_PLAN §9.1 Phase 3 扩展）
		Action: "assistant.tool.executed", Target: toolName,
		Reason: "read-only tool call",
		Metadata: datatypes.JSON(sprintJSON(map[string]any{
			"runId": run.ID, "args": joinArgs(args),
		})),
	})
}

func (r *Runner) modelID() string {
	if r.modelName != "" {
		return r.modelName
	}
	return "unknown"
}
