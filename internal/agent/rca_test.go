package agent

import (
	"context"
	"errors"
	"testing"

	"aiops/internal/ai"
	"aiops/internal/alert"
	"aiops/internal/incident"
)

// stubLLM 可编程的 RCALLM fake：记录输入供断言。
type stubLLM struct {
	out   *ai.RCAOutput
	err   error
	calls int
	last  ai.RCAInput
}

func (s *stubLLM) EnhanceRCA(_ context.Context, in ai.RCAInput) (*ai.RCAOutput, error) {
	s.calls++
	s.last = in
	return s.out, s.err
}

// rcaAlert 构造带 service 与 summary 的告警（进 collect 分支的输入形状）。
func rcaAlert(t *testing.T) *alert.Alert {
	t.Helper()
	return &alert.Alert{
		ID: "alert-1", Name: "order-slow", Severity: alert.SeverityMedium,
		Labels: mustJSON(t, map[string]any{"service": "order", "pod": "order-1"}),
		Event:  mustJSON(t, map[string]any{"annotations": map[string]any{"summary": "order latency spike"}}),
	}
}

func TestRCAAgentWithoutLLMNoEnhancement(t *testing.T) {
	r := NewRCAAgent(nil, nil)
	a := &alert.Alert{
		ID: "alert-1", Name: "order-slow", Severity: alert.SeverityMedium,
		Labels: mustJSON(t, map[string]any{"severity": "medium"}), // 无 service → service_not_found 分支，不触 topo
	}
	st := incident.NewState("default", a.ID)
	if err := r.Handle(t.Context(), &Context{State: st, Alert: a}); err != nil {
		t.Fatalf("rca.Handle error = %v", err)
	}
	res := st.NodeResult("rca")
	if res["rootCause"] != "service_not_found" {
		t.Errorf("rootCause = %v, want service_not_found", res["rootCause"])
	}
	if _, ok := res["llmRootCause"]; ok {
		t.Error("未配置 LLM 时不应产生 llm* 字段")
	}
}

func TestEnhanceAddsLLMResult(t *testing.T) {
	llm := &stubLLM{out: &ai.RCAOutput{
		RootCause: "order 数据库主从延迟", Rationale: "近期变更 v1.2.3 导致慢查询",
		SuggestedActions: []string{"回滚发布", "主从切换"},
	}}
	r := &RCAAgent{llm: llm}
	a := rcaAlert(t)
	st := incident.NewState("default", a.ID)
	result := map[string]any{
		"service": "order", "rootCause": "recent_change", "confidence": 0.54,
		"evidence": []string{"受影响服务存在近期变更：v1.2.3"},
		"changes":  []string{"v1.2.3"}, "paths": []string{"order → db"},
	}

	r.enhance(t.Context(), &Context{State: st, Alert: a}, "order", result)

	if llm.calls != 1 {
		t.Fatalf("llm.calls = %d, want 1", llm.calls)
	}
	if result["llmRootCause"] != "order 数据库主从延迟" {
		t.Errorf("llmRootCause = %v", result["llmRootCause"])
	}
	if result["llmRationale"] != "近期变更 v1.2.3 导致慢查询" {
		t.Errorf("llmRationale = %v", result["llmRationale"])
	}
	if result["llmAnalyzedAt"] == "" {
		t.Error("llmAnalyzedAt 不应为空")
	}
	// 确定性字段保留
	if result["rootCause"] != "recent_change" {
		t.Errorf("确定性 rootCause 被覆盖 = %v", result["rootCause"])
	}
	// LLM 输入组装正确
	if llm.last.Service != "order" || llm.last.AlertName != "order-slow" {
		t.Errorf("llm input = %+v", llm.last)
	}
	if llm.last.Summary != "order latency spike" {
		t.Errorf("summary = %q, want order latency spike", llm.last.Summary)
	}
	if llm.last.Confidence != 0.54 {
		t.Errorf("confidence = %v, want 0.54", llm.last.Confidence)
	}
	if len(llm.last.Changes) != 1 || llm.last.Changes[0] != "v1.2.3" {
		t.Errorf("changes = %v, want [v1.2.3]", llm.last.Changes)
	}
	if llm.last.Labels["pod"] != "order-1" {
		t.Errorf("labels = %v", llm.last.Labels)
	}
}

func TestEnhanceFailOpenOnLLMError(t *testing.T) {
	llm := &stubLLM{err: errors.New("connection refused")}
	r := &RCAAgent{llm: llm}
	a := rcaAlert(t)
	result := map[string]any{"rootCause": "recent_change", "confidence": 0.54}

	r.enhance(t.Context(), &Context{Alert: a}, "order", result)

	if llm.calls != 1 {
		t.Fatalf("llm.calls = %d, want 1", llm.calls)
	}
	for _, k := range []string{"llmRootCause", "llmRationale", "llmSuggestedActions"} {
		if _, ok := result[k]; ok {
			t.Errorf("LLM 失败后不应写入 %s", k)
		}
	}
}

func TestEnhanceIgnoresEmptyOutput(t *testing.T) {
	llm := &stubLLM{out: nil}
	r := &RCAAgent{llm: llm}
	result := map[string]any{"rootCause": "recent_change", "confidence": 0.54}

	r.enhance(t.Context(), &Context{Alert: rcaAlert(t)}, "order", result)
	if llm.calls != 1 {
		t.Fatalf("llm.calls = %d, want 1", llm.calls)
	}
	if _, ok := result["llmRootCause"]; ok {
		t.Error("out 为 nil 时不应写入 llm* 字段")
	}
}
