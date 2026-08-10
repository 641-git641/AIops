package assistant

import (
	"strings"
	"testing"

	"aiops/internal/incident"
)

// ---- validateToolArgs ----

func TestValidateToolArgs(t *testing.T) {
	schema := schema(map[string]any{
		"status":   enumProp("s", "firing", "resolved"),
		"severity": strProp("sev"),
		"limit":    intProp("l", 1, 50),
	}, "severity")

	cases := []struct {
		name string
		args map[string]any
		ok   bool
	}{
		{"合法", map[string]any{"severity": "critical", "limit": 10, "status": "firing"}, true},
		{"必填缺失", map[string]any{"limit": 10}, false},
		{"未知字段", map[string]any{"severity": "x", "hack": "1"}, false},
		{"枚举外", map[string]any{"severity": "x", "status": "boom"}, false},
		{"类型错", map[string]any{"severity": "x", "limit": "ten"}, false},
		{"整数小数", map[string]any{"severity": "x", "limit": 3.5}, false},
		{"空参数", map[string]any{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateToolArgs(schema, tc.args)
			if tc.ok && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

// ---- context：BuildMessages / trimHistory ----

func TestBuildMessagesSystemFirst(t *testing.T) {
	msgs, _ := BuildMessages("policy", "", []AssistantMessage{
		{Role: MsgUser, Content: "q1", Sequence: 1},
		{Role: MsgAssistant, Content: "a1", Sequence: 2},
		{Role: MsgUser, Content: "q2", Sequence: 3},
	}, nil)
	if len(msgs) != 4 || msgs[0].Role != "system" || msgs[0].Content != "policy" {
		t.Fatalf("msgs = %+v", msgs)
	}
	if msgs[3].Content != "q2" {
		t.Fatalf("最后一条应为 q2: %+v", msgs[3])
	}
}

func TestBuildMessagesContextSummaryInjected(t *testing.T) {
	msgs, _ := BuildMessages("policy", "当前绑定事故 xxx（critical/open）", []AssistantMessage{
		{Role: MsgUser, Content: "q1", Sequence: 1},
	}, nil)
	if len(msgs) != 2 || msgs[0].Role != "system" || !strings.Contains(msgs[0].Content, "当前绑定事故 xxx") {
		t.Fatalf("context 摘要未注入 system: %+v", msgs[0])
	}
}

func TestBuildMessagesSkipsEmptyAssistant(t *testing.T) {
	// 工具轮 assistant 消息 content 为空（只有 tool_calls）→ 不作为历史
	msgs, _ := BuildMessages("p", "", []AssistantMessage{
		{Role: MsgUser, Content: "q", Sequence: 1},
		{Role: MsgAssistant, Content: "", Sequence: 2}, // 工具轮
		{Role: MsgUser, Content: "q2", Sequence: 3},
	}, nil)
	if len(msgs) != 3 {
		t.Fatalf("msgs = %+v（空 assistant 应被跳过）", msgs)
	}
}

func TestTrimHistoryOldestFirst(t *testing.T) {
	hist := make([]AssistantMessage, 0, 50)
	for i := 0; i < 50; i++ {
		hist = append(hist, AssistantMessage{Role: MsgUser, Content: strings.Repeat("长", 100), Sequence: i})
	}
	kept, dropped := trimHistory(hist, 1000)
	if len(kept) >= len(hist) {
		t.Fatalf("裁剪无效: %d/%d", len(kept), len(hist))
	}
	if len(dropped) == 0 {
		t.Fatalf("应有被裁历史")
	}
	// 保留的是最近消息
	if kept[0].Sequence != hist[len(hist)-len(kept)].Sequence {
		t.Fatalf("应从旧到新裁: first seq = %d", kept[0].Sequence)
	}
	if kept[len(kept)-1].Sequence != 49 {
		t.Fatalf("最后一条应为最新: %d", kept[len(kept)-1].Sequence)
	}
}

func TestCompressDroppedSummary(t *testing.T) {
	dropped := []AssistantMessage{
		{Role: MsgUser, Content: "很长的早期问题描述" + strings.Repeat("字", 200), Sequence: 1},
		{Role: MsgAssistant, Content: "很长的早期回答" + strings.Repeat("字", 200), Sequence: 2},
		{Role: MsgSystem, Content: "不应出现", Sequence: 0},
	}
	s := compressDropped(dropped)
	if !strings.Contains(s, "早期对话已省略") || !strings.Contains(s, "user:") || !strings.Contains(s, "assistant:") {
		t.Fatalf("压缩摘要内容异常: %s", s)
	}
	if strings.Contains(s, "不应出现") {
		t.Fatalf("system 角色不应进入摘要: %s", s)
	}
	if len([]rune(s)) > 600 {
		t.Fatalf("摘要超预算: %d rune", len([]rune(s)))
	}
}

func TestBuildMessagesCompressionInjected(t *testing.T) {
	// 历史超预算 → 被裁部分生成早期摘要 system 消息
	hist := make([]AssistantMessage, 0, 30)
	for i := 0; i < 30; i++ {
		hist = append(hist, AssistantMessage{Role: MsgUser, Content: strings.Repeat("字", 800), Sequence: i})
	}
	msgs, _ := BuildMessages("p", "", hist, nil)
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Content, "早期对话已省略") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应注入早期摘要 system 消息: %d msgs", len(msgs))
	}
}

func TestTrimToolResultRuneSafe(t *testing.T) {
	s := strings.Repeat("汉字", 10000)
	got := trimToolResult(s)
	if len([]rune(got)) > MaxResultBytes+10 {
		t.Fatalf("裁剪超限: %d", len([]rune(got)))
	}
}

// ---- tools_read：summarizeNodeResults 四节点模板 ----

func TestSummarizeNodeResults(t *testing.T) {
	st := &incident.State{}
	st.SetNodeResult("monitor", map[string]any{
		"severity": "critical", "service": "pay", "alertName": "pay-down", "fingerprint": "fp-1",
	})
	st.SetNodeResult("rca", map[string]any{
		"service": "pay", "confidence": 0.6, "rootCause": "db conn exhausted",
		"evidence": []string{"e1"}, "suggestedActions": []string{"scale"}, "llmRootCause": "llm 推测",
	})
	st.SetNodeResult("heal", map[string]any{
		"status": "awaiting_approval", "action": "scale db", "level": "L1", "blastRadius": 0.35,
		"approvalRequired": true, "dryRun": false,
	})
	st.SetNodeResult("change", map[string]any{
		"action": "recent v1.2.3", "riskScore": 0.8, "level": "L2",
		"breakdown": map[string]any{"blastRadius": 0.5},
	})

	out := summarizeNodeResults(st)
	rca, _ := out["rca"].(map[string]any)
	if rca["rootCause"] != "db conn exhausted" || rca["llmRootCause"] != "llm 推测" {
		t.Fatalf("rca = %+v", rca)
	}
	heal, _ := out["heal"].(map[string]any)
	if heal["status"] != "awaiting_approval" || heal["level"] != "L1" {
		t.Fatalf("heal = %+v", heal)
	}
	mon, _ := out["monitor"].(map[string]any)
	if mon["severity"] != "critical" {
		t.Fatalf("monitor = %+v", mon)
	}
	// nil 安全
	if summarizeNodeResults(nil) != nil {
		t.Fatal("nil state 应返回 nil")
	}
	// 空 state（无节点）不 panic
	empty := summarizeNodeResults(&incident.State{})
	if len(empty) != 0 {
		t.Fatalf("empty = %+v", empty)
	}
}
