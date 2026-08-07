package workflow

import (
	"encoding/json"
	"fmt"
	"testing"

	"aiops/internal/alert"
)

func mkAlert(name, svc, sev, provider string) *alert.Alert {
	b, _ := json.Marshal(map[string]any{"service": svc})
	return &alert.Alert{
		ID: "al-" + name, Name: name, Severity: sev, Provider: provider,
		Status: alert.StatusFiring, Fingerprint: "fp-" + name + "-" + svc, Labels: b,
	}
}

func TestParse_Valid(t *testing.T) {
	def, err := Parse([]byte(defaultWorkflowYAML))
	if err != nil {
		t.Fatalf("parse default workflow: %v", err)
	}
	if def.Workflow.ID != "order-medium-agent-demo" {
		t.Errorf("id = %q", def.Workflow.ID)
	}
	if len(def.Workflow.Triggers) != 1 || len(def.Workflow.Triggers[0].Filters) != 2 {
		t.Errorf("triggers/filters = %+v", def.Workflow.Triggers)
	}
	if len(def.Workflow.Steps) != 1 || len(def.Workflow.Actions) != 1 {
		t.Errorf("steps=%d actions=%d", len(def.Workflow.Steps), len(def.Workflow.Actions))
	}
	if def.Workflow.Actions[0].Provider.Type != "agent" {
		t.Errorf("action provider = %q, want agent", def.Workflow.Actions[0].Provider.Type)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"missing id", "workflow:\n  name: x\n  triggers:\n    - type: alert\n"},
		{"no trigger", "workflow:\n  id: x\n"},
		{"step no provider", "workflow:\n  id: x\n  triggers:\n    - type: alert\n  steps:\n    - name: s\n"},
		{"bad yaml", "workflow: [unclosed"},
	}
	for _, c := range cases {
		if _, err := Parse([]byte(c.yaml)); err == nil {
			t.Errorf("%s: want error, got nil", c.name)
		}
	}
}

func TestMatchesTrigger(t *testing.T) {
	a := mkAlert("pay-slow", "pay", alert.SeverityCritical, "prometheus")

	// 无 filters → 全命中
	if !MatchesTrigger(Trigger{Type: "alert"}, a) {
		t.Error("no filters should match all")
	}
	// 精确匹配 service + severity
	ok := MatchesTrigger(Trigger{Type: "alert", Filters: []Filter{
		{Key: "service", Value: "pay"}, {Key: "severity", Value: "critical"},
	}}, a)
	if !ok {
		t.Error("exact service+severity should match")
	}
	if MatchesTrigger(Trigger{Type: "alert", Filters: []Filter{{Key: "service", Value: "order"}}}, a) {
		t.Error("service=order should not match pay alert")
	}
	// severity >= / <= 运算符
	if !MatchesTrigger(Trigger{Type: "alert", Filters: []Filter{{Key: "severity", Value: ">=high"}}}, a) {
		t.Error("critical should satisfy >=high")
	}
	if MatchesTrigger(Trigger{Type: "alert", Filters: []Filter{{Key: "severity", Value: ">=critical"}}}, mkAlert("x", "y", alert.SeverityHigh, "p")) {
		t.Error("high should not satisfy >=critical")
	}
	// name / provider
	if !MatchesTrigger(Trigger{Type: "alert", Filters: []Filter{{Key: "name", Value: "pay-slow"}}}, a) {
		t.Error("name match failed")
	}
	if !MatchesTrigger(Trigger{Type: "alert", Filters: []Filter{{Key: "provider", Value: "prometheus"}}}, a) {
		t.Error("provider match failed")
	}
	// 非 alert 触发不支持
	if MatchesTrigger(Trigger{Type: "interval"}, a) {
		t.Error("interval trigger should not match (MVP)")
	}
	// 任意 labels 键（裸键查 labels map）
	if !MatchesTrigger(Trigger{Type: "alert", Filters: []Filter{{Key: "service", Value: "pay"}}}, a) {
		t.Error("裸 service 键应匹配 labels.service")
	}
	if MatchesTrigger(Trigger{Type: "alert", Filters: []Filter{{Key: "env", Value: "prod"}}}, a) {
		t.Error("不存在的标签键不应命中")
	}
}

func TestDedupKey(t *testing.T) {
	k1 := DedupKey("wf-1", "fp-a")
	k2 := DedupKey("wf-1", "fp-a")
	if k1 != k2 {
		t.Errorf("dedup key unstable across calls: %s != %s", k1, k2)
	}
	if DedupKey("wf-1", "fp-a") == DedupKey("wf-2", "fp-a") {
		t.Error("different workflow should differ")
	}
	if DedupKey("wf-1", "fp-a") == DedupKey("wf-1", "fp-b") {
		t.Error("different fingerprint should differ")
	}
	if len(k1) != 64 {
		t.Errorf("sha256 hex len = %d, want 64", len(k1))
	}
}

func TestRunSteps(t *testing.T) {
	a := mkAlert("pay-slow", "pay", "medium", "prometheus")
	order := []string{}
	dispatch := func(s Step, al *alert.Alert) (map[string]any, error) {
		order = append(order, s.Name)
		if s.Name == "boom" {
			return nil, fmt.Errorf("boom")
		}
		return map[string]any{"ran": s.Name}, nil
	}
	steps := []Step{{Name: "s1", Provider: Provider{Type: "log"}}, {Name: "s2", Provider: Provider{Type: "log"}}}
	actions := []Step{{Name: "a1", Provider: Provider{Type: "agent"}}}

	// 正常：steps 后 actions
	results, err := runSteps(steps, actions, a, dispatch)
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if fmt.Sprint(order) != "[s1 s2 a1]" {
		t.Errorf("order = %v, want [s1 s2 a1] (steps 先于 actions)", order)
	}
	if len(results) != 3 {
		t.Errorf("results = %v", results)
	}

	// 失败即停：第 2 步失败 → a1 不执行
	order = nil
	bad := []Step{{Name: "s1", Provider: Provider{Type: "log"}}, {Name: "boom", Provider: Provider{Type: "log"}}}
	if _, err := runSteps(bad, actions, a, dispatch); err == nil {
		t.Error("want error on failing step")
	}
	if fmt.Sprint(order) != "[s1 boom]" {
		t.Errorf("order after failure = %v, want [s1 boom] (fail-fast)", order)
	}
}

func TestRenderTemplate(t *testing.T) {
	a := mkAlert("pay-slow", "pay", "critical", "prometheus")
	got := renderTemplate("告警 {{.alert.name}} 严重度 {{.alert.severity}}", a)
	want := "告警 pay-slow 严重度 critical"
	if got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
	// 未定义字段不报错（missingkey=zero；map 零值为 nil → 渲染 <no value>）
	if got := renderTemplate("{{.alert.nope}}", a); got != "<no value>" {
		t.Errorf("missing field = %q, want <no value> (missingkey=zero)", got)
	}
	// 空串 / 解析失败回退原文
	if renderTemplate("", a) != "" {
		t.Error("empty template should stay empty")
	}
	if got := renderTemplate("{{bad", a); got != "{{bad" {
		t.Errorf("parse fail should return raw, got %q", got)
	}
}
