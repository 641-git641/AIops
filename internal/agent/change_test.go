package agent

import (
	"testing"

	"aiops/internal/alert"
	"aiops/internal/incident"
)

func TestChangeRiskScoreL2ForCriticalRollback(t *testing.T) {
	st := incident.NewState("default", "alert-1")
	// heal 提议 rollback（操作风险 0.3），爆炸半径 0.35（风险 0.7），critical（1.0）
	st.SetNodeResult("heal", map[string]any{"action": "rollback", "blastRadius": 0.35})
	st.SetNodeResult("rca", map[string]any{"changes": []any{"v1.2.3", "v1.2.4"}})
	a := &alert.Alert{ID: "alert-1", Severity: alert.SeverityCritical,
		Labels: mustJSON(t, map[string]any{"service": "pay"})}

	c := NewChangeAgent()
	if err := c.Handle(t.Context(), &Context{State: st, Alert: a}); err != nil {
		t.Fatalf("change.Handle error = %v", err)
	}
	res := st.NodeResult("change")
	if res == nil {
		t.Fatal("change node result missing")
	}
	// 期望风险: blast 0.7*0.3 + op 0.3*0.25 + time 0.2*0.2 + history 0.8*0.15 + crit 1.0*0.10
	//        = 0.21 + 0.075 + 0.04 + 0.12 + 0.10 = 0.545 → L2
	score, _ := res["riskScore"].(float64)
	if score < 0.5 {
		t.Errorf("riskScore = %v, want >= 0.5 (L2)", score)
	}
	if res["level"] != "L2" {
		t.Errorf("level = %v, want L2", res["level"])
	}
	if res["approvalRequired"] != true {
		t.Errorf("approvalRequired = %v, want true", res["approvalRequired"])
	}
	if st.Status != incident.StatePendingApproval {
		t.Errorf("state status = %q, want pending_approval", st.Status)
	}
}

func TestChangeRiskScoreL0ForLowRiskVerify(t *testing.T) {
	st := incident.NewState("default", "alert-1")
	st.SetNodeResult("heal", map[string]any{"action": "verify_dependency", "blastRadius": 0.02})
	st.SetNodeResult("rca", map[string]any{"changes": []any{}})
	a := &alert.Alert{ID: "alert-1", Severity: alert.SeverityLow,
		Labels: mustJSON(t, map[string]any{"service": "pay"})}

	c := NewChangeAgent()
	if err := c.Handle(t.Context(), &Context{State: st, Alert: a}); err != nil {
		t.Fatalf("change.Handle error = %v", err)
	}
	res := st.NodeResult("change")
	if res["level"] != "L0" {
		t.Errorf("level = %v, want L0", res["level"])
	}
	if res["approvalRequired"] != false {
		t.Errorf("approvalRequired = %v, want false", res["approvalRequired"])
	}
	if res["decision"] != "auto_execute" {
		t.Errorf("decision = %v, want auto_execute", res["decision"])
	}
	if st.Status == incident.StatePendingApproval {
		t.Error("L0 change must not enter pending_approval")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		score        float64
		wantLevel    string
		wantDecision string
	}{
		{0.2, "L0", "auto_execute"},
		{0.3, "L0", "auto_execute"},
		{0.35, "L1", "await_approval"},
		{0.5, "L1", "await_approval"},
		{0.51, "L2", "await_approval"},
		{0.9, "L2", "await_approval"},
	}
	for _, tc := range cases {
		lv, dec := classify(tc.score)
		if lv != tc.wantLevel || dec != tc.wantDecision {
			t.Errorf("classify(%.2f) = (%s, %s), want (%s, %s)",
				tc.score, lv, dec, tc.wantLevel, tc.wantDecision)
		}
	}
}

func TestCriticalityRisk(t *testing.T) {
	if got := criticalityRisk(severityRank(alert.SeverityCritical)); got != 1.0 {
		t.Errorf("critical rank risk = %v, want 1.0", got)
	}
	if got := criticalityRisk(severityRank(alert.SeverityHigh)); got != 0.7 {
		t.Errorf("high rank risk = %v, want 0.7", got)
	}
	if got := criticalityRisk(severityRank("")); got != 0.3 {
		t.Errorf("empty severity risk = %v, want 0.3", got)
	}
}
