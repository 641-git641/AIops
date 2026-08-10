package assistant_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aiops/internal/alert"
	"aiops/internal/assistant"
	"aiops/internal/eventbus"
	"aiops/internal/incident"
	"aiops/internal/llm"
	"aiops/internal/overview"
	"aiops/internal/topology"
	"aiops/internal/util"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// fakeModel 可编程生成模型（按调用次数依次返回预置响应）。
type fakeModel struct {
	responses []*llm.Completion
	block     chan struct{} // 非 nil 时挂起（模拟卡死）
	calls     int
}

func (f *fakeModel) Chat(ctx context.Context, _ llm.ChatRequest, _ func(llm.Delta)) (*llm.Completion, error) {
	f.calls++
	if f.block != nil {
		select {
		case <-f.block:
			return nil, errors.New("fake: blocked")
		case <-ctx.Done():
			return nil, ctx.Err() // 响应 run 超时取消
		}
	}
	if len(f.responses) == 0 {
		return nil, errors.New("fake: no response")
	}
	if f.calls > len(f.responses) {
		return &llm.Completion{Content: "最终回答（兜底）", FinishReason: llm.FinishStop}, nil
	}
	return f.responses[f.calls-1], nil
}

func toolCallResp(name, args string) *llm.Completion {
	return &llm.Completion{
		FinishReason: llm.FinishToolCalls,
		ToolCalls: []llm.ToolCall{{
			ID: "call-t1", Type: "function",
			Function: llm.ToolCallFunc{Name: name, Arguments: args},
		}},
	}
}

// buildRunner 组装 runner（真实领域服务 + 真实工具注册表 + 内存 broker）。
func buildRunner(t *testing.T, model assistant.ChatModel, limits assistant.Limits) (*assistant.Runner, *assistant.Broker) {
	t.Helper()
	db := openDB(t)
	deps := assistant.ToolDeps{
		Overview:  overview.New(db, nil, nil, nil, nil, time.Now()),
		Alerts:    alert.NewRepository(db),
		Incidents: incident.NewService(db),
		Topology:  topology.NewService(db),
	}
	reg := assistant.NewRegistry(deps)
	broker := assistant.NewBroker(db)
	audit := eventbus.NewAuditService(db)
	runner := assistant.NewRunner(db, model, "mock", reg, broker, audit, limits, nil)
	return runner, broker
}

// seedRun 入队一个 run（返回 id）。
func seedRun(t *testing.T, db *gorm.DB, tenant, threadID string) string {
	t.Helper()
	run := &assistant.AssistantRun{
		ID: util.NewID(), TenantID: tenant, ThreadID: threadID,
		Model: "mock", PromptVersion: "v1", IdempotencyKey: util.NewID(),
	}
	id, err := assistant.EnqueueRun(context.Background(), db, run)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// runAndClaim 入队 → 领取 → 手动结算（返回 run 最终状态）。
func runAndClaim(t *testing.T, db *gorm.DB, runner *assistant.Runner, tenant, threadID string) string {
	t.Helper()
	ctx := context.Background()
	runID := seedRun(t, db, tenant, threadID)
	claims, err := assistant.ClaimRuns(ctx, db, "test-worker", tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	// 可能领到其他测试残留的过期 run——只处理目标线程的
	var target *assistant.AssistantRun
	for i := range claims {
		if claims[i].ID == runID {
			target = &claims[i]
			break
		}
	}
	if target == nil {
		t.Fatalf("目标 run 未被领取: claims = %+v", claims)
	}
	runner.ProcessClaimed(ctx, target)
	return runID
}

func TestRunnerSingleTurn(t *testing.T) {
	db := openDB(t)
	tenant := "asst-run1"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	seq, _ := assistant.NextMessageSequence(ctx, db, tenant, th.ID)
	_ = assistant.CreateMessage(ctx, db, &assistant.AssistantMessage{
		ID: util.NewID(), TenantID: tenant, ThreadID: th.ID,
		Role: assistant.MsgUser, Content: "你好", Sequence: seq,
	})

	model := &fakeModel{responses: []*llm.Completion{{Content: "平台正常", FinishReason: llm.FinishStop}}}
	runner, _ := buildRunner(t, model, testLimits())
	runID := runAndClaim(t, db, runner, tenant, th.ID)

	got, _ := assistant.GetRun(ctx, db, tenant, runID)
	if got.Status != assistant.RunCompleted {
		t.Fatalf("status = %s, want completed（errorCode=%s）", got.Status, got.ErrorCode)
	}
	msgs, _ := assistant.ListMessages(ctx, db, tenant, th.ID)
	if len(msgs) != 2 || msgs[1].Role != assistant.MsgAssistant || msgs[1].Content != "平台正常" {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestRunnerToolLoop(t *testing.T) {
	db := openDB(t)
	tenant := "asst-run2"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	seq, _ := assistant.NextMessageSequence(ctx, db, tenant, th.ID)
	_ = assistant.CreateMessage(ctx, db, &assistant.AssistantMessage{
		ID: util.NewID(), TenantID: tenant, ThreadID: th.ID,
		Role: assistant.MsgUser, Content: "列出最近 critical 告警", Sequence: seq,
	})

	// 造一条 critical 告警（工具查询的数据源）
	if err := db.Create(&alert.Alert{
		ID: util.NewID(), TenantID: tenant, Provider: "test", Name: "pay-down",
		Severity: "critical", Status: "firing", Fingerprint: "fp-" + tenant,
		AlertHash: "h", Labels: datatypes.JSON(`{"service":"pay"}`), Event: datatypes.JSON(`{}`),
		StartedAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
		CreatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}

	// 第一轮 tool_calls（list_alerts），第二轮最终回答
	model := &fakeModel{responses: []*llm.Completion{
		toolCallResp("list_alerts", `{"severity":"critical","limit":5}`),
		{Content: "共 3 条 critical 告警", FinishReason: llm.FinishStop},
	}}
	runner, _ := buildRunner(t, model, testLimits())
	runID := runAndClaim(t, db, runner, tenant, th.ID)

	got, _ := assistant.GetRun(ctx, db, tenant, runID)
	if got.Status != assistant.RunCompleted {
		t.Fatalf("status = %s, want completed（errorCode=%s）", got.Status, got.ErrorCode)
	}
	steps, _ := assistant.ListSteps(ctx, db, tenant, runID)
	var toolStep *assistant.AssistantStep
	for i := range steps {
		if steps[i].ToolName == "list_alerts" {
			toolStep = &steps[i]
		}
	}
	if toolStep == nil {
		t.Fatalf("steps = %+v（无 list_alerts 步骤）", steps)
	}
	if !strings.Contains(string(toolStep.ResultSummary), "critical") {
		t.Fatalf("工具结果摘要应含 critical: %s", toolStep.ResultSummary)
	}
	msgs, _ := assistant.ListMessages(ctx, db, tenant, th.ID)
	last := msgs[len(msgs)-1]
	if last.Role != assistant.MsgAssistant || !strings.Contains(last.Content, "critical") {
		t.Fatalf("最终消息 = %+v", last)
	}
}

func TestRunnerUnknownToolSafeFailure(t *testing.T) {
	db := openDB(t)
	tenant := "asst-run3"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	seq, _ := assistant.NextMessageSequence(ctx, db, tenant, th.ID)
	_ = assistant.CreateMessage(ctx, db, &assistant.AssistantMessage{
		ID: util.NewID(), TenantID: tenant, ThreadID: th.ID,
		Role: assistant.MsgUser, Content: "执行 rm -rf", Sequence: seq,
	})

	model := &fakeModel{responses: []*llm.Completion{
		toolCallResp("rm_rf", `{}`), // 未知工具
		{Content: "安全拒绝", FinishReason: llm.FinishStop},
	}}
	runner, _ := buildRunner(t, model, testLimits())
	runID := runAndClaim(t, db, runner, tenant, th.ID)

	got, _ := assistant.GetRun(ctx, db, tenant, runID)
	if got.Status != assistant.RunCompleted {
		t.Fatalf("未知工具不应使 run 失败: %s（%s）", got.Status, got.ErrorCode)
	}
	steps, _ := assistant.ListSteps(ctx, db, tenant, runID)
	found := false
	for _, s := range steps {
		if s.ToolName == "rm_rf" && s.Status == "rejected" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未知工具应记录 rejected 步骤: %+v", steps)
	}
}

func TestRunnerModelNotConfigured(t *testing.T) {
	db := openDB(t)
	tenant := "asst-run4"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	seq, _ := assistant.NextMessageSequence(ctx, db, tenant, th.ID)
	_ = assistant.CreateMessage(ctx, db, &assistant.AssistantMessage{
		ID: util.NewID(), TenantID: tenant, ThreadID: th.ID,
		Role: assistant.MsgUser, Content: "q", Sequence: seq,
	})

	runner, _ := buildRunner(t, nil, testLimits()) // model = nil
	runID := runAndClaim(t, db, runner, tenant, th.ID)

	got, _ := assistant.GetRun(ctx, db, tenant, runID)
	if got.Status != assistant.RunFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if got.Attempts > 1 {
		t.Fatalf("attempts = %d（未配置模型不应重试）", got.Attempts)
	}
}

func TestRunnerRoundBudget(t *testing.T) {
	db := openDB(t)
	tenant := "asst-run5"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	seq, _ := assistant.NextMessageSequence(ctx, db, tenant, th.ID)
	_ = assistant.CreateMessage(ctx, db, &assistant.AssistantMessage{
		ID: util.NewID(), TenantID: tenant, ThreadID: th.ID,
		Role: assistant.MsgUser, Content: "循环调用", Sequence: seq,
	})

	// 每轮都返回 tool_calls（永远不结束）→ 预算终止（MaxModelRounds=2，两轮都 tool_calls）
	model := &fakeModel{responses: []*llm.Completion{
		toolCallResp("get_platform_overview", `{}`),
		toolCallResp("get_platform_overview", `{}`),
	}}
	limits := testLimits()
	limits.MaxModelRounds = 2
	runner, _ := buildRunner(t, model, limits)
	runID := runAndClaim(t, db, runner, tenant, th.ID)

	got, _ := assistant.GetRun(ctx, db, tenant, runID)
	if got.Status != assistant.RunFailed {
		t.Fatalf("预算耗尽应失败: %s", got.Status)
	}
	if !strings.Contains(got.ErrorCode, "budget") {
		t.Fatalf("errorCode = %s", got.ErrorCode)
	}
}

func TestRunnerTimeout(t *testing.T) {
	db := openDB(t)
	tenant := "asst-run6"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	seq, _ := assistant.NextMessageSequence(ctx, db, tenant, th.ID)
	_ = assistant.CreateMessage(ctx, db, &assistant.AssistantMessage{
		ID: util.NewID(), TenantID: tenant, ThreadID: th.ID,
		Role: assistant.MsgUser, Content: "卡住", Sequence: seq,
	})

	model := &fakeModel{block: make(chan struct{})} // 卡死
	limits := testLimits()
	limits.RunTimeout = 200 * time.Millisecond
	runner, _ := buildRunner(t, model, limits)
	runID := seedRun(t, db, tenant, th.ID)
	claims, _ := assistant.ClaimRuns(context.Background(), db, "test-worker", tenant, 10)

	done := make(chan struct{})
	go func() {
		runner.ProcessClaimed(context.Background(), &claims[0])
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run 超时未返回")
	}
	got, _ := assistant.GetRun(context.Background(), db, tenant, runID)
	if got.Status != assistant.RunFailed {
		t.Fatalf("status = %s, want failed（超时）", got.Status)
	}
}

// testLimits 测试用默认限制。
func testLimits() assistant.Limits {
	return assistant.Limits{
		MaxConcurrency: 4, MaxModelRounds: 8, MaxToolCalls: 12,
		RunTimeout: 5 * time.Second, ToolTimeout: 3 * time.Second,
	}
}
