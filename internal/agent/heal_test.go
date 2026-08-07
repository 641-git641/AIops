package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"aiops/internal/alert"
	"aiops/internal/circuit"
	"aiops/internal/incident"
	"aiops/internal/playbook"
	"aiops/internal/topology"
)

// fakePb 剧本匹配 fake（注入 HealAgent 的 playbookFinder）。
type fakePb struct {
	pb  *playbook.Playbook
	err error
}

func (f *fakePb) Resolve(_ context.Context, _ string, _ []string) (*playbook.Playbook, error) {
	return f.pb, f.err
}

// fakeTopo 拓扑查询 fake。
type fakeTopo struct {
	t *topology.TopologyService
}

func (f *fakeTopo) Get(_ context.Context, _, _ string) (*topology.TopologyService, error) {
	return f.t, nil
}

func healCfg(dryRun bool) Cfg {
	return Cfg{DryRun: dryRun, BlastRadiusLimit: 0.2, AutoHealLevel: "L0"}
}

// seedRCA 在 state 里预置 RCA 结果（heal 的输入）。
func seedRCA(t *testing.T, st *incident.State, actions []string) {
	t.Helper()
	st.SetNodeResult("rca", map[string]any{
		"rootCause":        "recent_change",
		"confidence":       0.69,
		"suggestedActions": actions,
	})
}

func newHealState() *incident.State {
	st := incident.NewState("default", "alert-1")
	return st
}

func TestHealDryRunRequiresApproval(t *testing.T) {
	h := NewHealAgent(&fakePb{pb: &playbook.Playbook{
		Name: "rollback", Level: playbook.LevelL1, BlastRadiusFactor: 0.35,
		CommandTemplate: "kubectl rollout undo deployment/{{.service}}", Enabled: true,
	}}, nil, nil)
	a := &alert.Alert{ID: "alert-1", Severity: alert.SeverityCritical,
		Labels: mustJSON(t, map[string]any{"service": "pay"})}
	st := newHealState()
	seedRCA(t, st, []string{"rollback", "profiling"})

	if err := h.Handle(t.Context(), &Context{State: st, Alert: a, Cfg: healCfg(true)}); err != nil {
		t.Fatalf("heal.Handle error = %v", err)
	}
	res := st.NodeResult("heal")
	if res == nil {
		t.Fatal("heal node result missing")
	}
	if res["status"] != "dry_run" {
		t.Errorf("status = %v, want dry_run", res["status"])
	}
	if res["approvalRequired"] != true {
		t.Errorf("approvalRequired = %v, want true", res["approvalRequired"])
	}
	if res["action"] != "rollback" {
		t.Errorf("action = %v, want rollback", res["action"])
	}
	if st.Status != incident.StatePendingApproval {
		t.Errorf("state status = %q, want pending_approval", st.Status)
	}
	// 命令模板渲染
	if res["command"] != "kubectl rollout undo deployment/pay" {
		t.Errorf("command = %v, want rendered with service", res["command"])
	}
}

func TestHealBlastRadiusDowngradeToL2(t *testing.T) {
	// restart_pod 固有 0.10，拓扑 impact 0.8 → blast = 0.10 + 0.16 = 0.26 > 0.2 → 降级 L2
	h := NewHealAgent(&fakePb{pb: &playbook.Playbook{
		Name: "restart_pod", Level: playbook.LevelL0, BlastRadiusFactor: 0.10, Enabled: true,
	}}, &fakeTopo{t: &topology.TopologyService{ImpactScore: 0.8}}, nil)
	a := &alert.Alert{ID: "alert-1", Severity: alert.SeverityCritical,
		Labels: mustJSON(t, map[string]any{"service": "pay"})}
	st := newHealState()
	seedRCA(t, st, []string{"restart_pod"})

	if err := h.Handle(t.Context(), &Context{State: st, Alert: a, Cfg: healCfg(true)}); err != nil {
		t.Fatalf("heal.Handle error = %v", err)
	}
	res := st.NodeResult("heal")
	if res["level"] != "L2" {
		t.Errorf("level = %v, want L2 (blast radius 超限降级)", res["level"])
	}
	if res["downgraded"] != true {
		t.Errorf("downgraded = %v, want true", res["downgraded"])
	}
	blast, _ := res["blastRadius"].(float64)
	if blast < 0.25 {
		t.Errorf("blastRadius = %v, want >= 0.26", blast)
	}
}

func TestHealAutoExecuteWhenL0AndNoDryRun(t *testing.T) {
	h := NewHealAgent(&fakePb{pb: &playbook.Playbook{
		Name: "restart_pod", Level: playbook.LevelL0, BlastRadiusFactor: 0.10, Enabled: true,
	}}, nil, circuit.NewBreaker(5, 60*time.Second))
	a := &alert.Alert{ID: "alert-1", Severity: alert.SeverityMedium,
		Labels: mustJSON(t, map[string]any{"service": "pay"})}
	st := newHealState()
	seedRCA(t, st, []string{"restart_pod"})

	cfg := healCfg(false) // dry_run=false → 允许自动执行
	if err := h.Handle(t.Context(), &Context{State: st, Alert: a, Cfg: cfg}); err != nil {
		t.Fatalf("heal.Handle error = %v", err)
	}
	res := st.NodeResult("heal")
	if res["status"] != "executed" {
		t.Errorf("status = %v, want executed", res["status"])
	}
	if res["approvalRequired"] != false {
		t.Errorf("approvalRequired = %v, want false", res["approvalRequired"])
	}
	if st.Status == incident.StatePendingApproval {
		t.Error("L0 auto-execute must not enter pending_approval")
	}
	if res["executedAt"] == "" {
		t.Error("executedAt must be set")
	}
}

func TestHealBlockedByOpenBreaker(t *testing.T) {
	br := circuit.NewBreaker(1, 60*time.Second) // 60s 冷却，确保 Handle 时仍处于 OPEN
	br.Allow()
	br.Failure() // 1 次失败 → OPEN
	if br.State() != circuit.StateOpen {
		t.Fatalf("breaker state = %v, want open", br.State())
	}
	h := NewHealAgent(&fakePb{pb: &playbook.Playbook{
		Name: "restart_pod", Level: playbook.LevelL0, BlastRadiusFactor: 0.10, Enabled: true,
	}}, nil, br)
	a := &alert.Alert{ID: "alert-1", Severity: alert.SeverityMedium,
		Labels: mustJSON(t, map[string]any{"service": "pay"})}
	st := newHealState()
	seedRCA(t, st, []string{"restart_pod"})

	if err := h.Handle(t.Context(), &Context{State: st, Alert: a, Cfg: healCfg(false)}); err != nil {
		t.Fatalf("heal.Handle error = %v", err)
	}
	res := st.NodeResult("heal")
	if res["status"] != "blocked" {
		t.Errorf("status = %v, want blocked", res["status"])
	}
}

func TestHealNoSuggestedActionFailOpen(t *testing.T) {
	h := NewHealAgent(&fakePb{}, nil, nil)
	a := &alert.Alert{ID: "alert-1", Severity: alert.SeverityHigh}
	st := newHealState()
	seedRCA(t, st, nil) // RCA 无建议动作

	if err := h.Handle(t.Context(), &Context{State: st, Alert: a, Cfg: healCfg(true)}); err != nil {
		t.Fatalf("heal.Handle must fail-open, got error = %v", err)
	}
	res := st.NodeResult("heal")
	if res["status"] != "no_suggested_action" {
		t.Errorf("status = %v, want no_suggested_action", res["status"])
	}
}

func TestSuggestedActionsMergesLLM(t *testing.T) {
	cases := []struct {
		name string
		rca  map[string]any
		want []string
	}{
		{
			name: "llm 建议并入确定性建议（去重保留顺序）",
			rca: map[string]any{
				"suggestedActions":    []any{"rollback", "restart"},
				"llmSuggestedActions": []any{"restart", "scale-out"},
			},
			want: []string{"rollback", "restart", "scale-out"},
		},
		{
			name: "仅 llm 建议（RCA 无确定性建议）",
			rca: map[string]any{
				"llmSuggestedActions": []any{"restart"},
			},
			want: []string{"restart"},
		},
		{
			name: "llm 建议为空/非字符串被忽略",
			rca: map[string]any{
				"suggestedActions":    []any{"restart", ""},
				"llmSuggestedActions": []any{42, "restart"},
			},
			want: []string{"restart"},
		},
		{name: "无建议", rca: nil, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := suggestedActions(tc.rca)
			if len(got) != len(tc.want) {
				t.Fatalf("suggestedActions = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("index %d = %q, want %q（%v vs %v）", i, got[i], tc.want[i], got, tc.want)
				}
			}
		})
	}
}

// recordingPb 记录 Resolve 收到的建议动作列表（用于断言 Heal 回填 LLM 建议）。
type recordingPb struct {
	pb  *playbook.Playbook
	got []string
}

func (f *recordingPb) Resolve(_ context.Context, _ string, suggested []string) (*playbook.Playbook, error) {
	f.got = append([]string(nil), suggested...)
	return f.pb, nil
}

func TestHealBackfillsLLMSuggestedActions(t *testing.T) {
	// 确定性建议无对应剧本，LLM 建议回填后被 Resolve 收到 → Heal 走 LLM 建议的剧本
	rp := &recordingPb{pb: &playbook.Playbook{
		Name: "rollback", Level: playbook.LevelL1, BlastRadiusFactor: 0.1,
		CommandTemplate: "kubectl rollout undo deployment/{{.service}}", Enabled: true,
	}}
	h := NewHealAgent(rp, nil, nil)
	a := &alert.Alert{ID: "alert-1", Severity: alert.SeverityHigh,
		Labels: mustJSON(t, map[string]any{"service": "pay"})}
	st := newHealState()
	st.SetNodeResult("rca", map[string]any{
		"rootCause":           "recent_change",
		"suggestedActions":    []any{"profiling"},           // 确定性：无 rollback 剧本
		"llmSuggestedActions": []any{"rollback", "restart"}, // LLM 补充：匹配 rollback 剧本
	})

	if err := h.Handle(t.Context(), &Context{State: st, Alert: a, Cfg: healCfg(true)}); err != nil {
		t.Fatalf("heal.Handle error = %v", err)
	}
	res := st.NodeResult("heal")
	if res["action"] != "rollback" {
		t.Errorf("action = %v, want rollback（LLM 建议回填后匹配到剧本）", res["action"])
	}
	if len(rp.got) != 3 || rp.got[0] != "profiling" || rp.got[1] != "rollback" || rp.got[2] != "restart" {
		t.Errorf("Resolve 收到建议 = %v, want [profiling rollback restart]", rp.got)
	}
}

func TestHealNoPlaybookFailOpen(t *testing.T) {
	h := NewHealAgent(&fakePb{err: errors.New("no playbook")}, nil, nil)
	a := &alert.Alert{ID: "alert-1", Severity: alert.SeverityHigh,
		Labels: mustJSON(t, map[string]any{"service": "pay"})}
	st := newHealState()
	seedRCA(t, st, []string{"custom_action"})

	if err := h.Handle(t.Context(), &Context{State: st, Alert: a, Cfg: healCfg(true)}); err != nil {
		t.Fatalf("heal.Handle must fail-open, got error = %v", err)
	}
	res := st.NodeResult("heal")
	if res["status"] != "no_playbook" {
		t.Errorf("status = %v, want no_playbook", res["status"])
	}
}
