package pipeline

import (
	"testing"

	"aiops/internal/alert"
	"aiops/internal/incident"
)

func TestSeverityRank(t *testing.T) {
	cases := []struct {
		sev  string
		want int
	}{
		{alert.SeverityCritical, 4},
		{alert.SeverityHigh, 3},
		{alert.SeverityMedium, 2},
		{alert.SeverityLow, 1},
		{"", 0},
	}
	for _, tc := range cases {
		if got := alert.SeverityRank(tc.sev); got != tc.want {
			t.Errorf("SeverityRank(%q) = %d, want %d", tc.sev, got, tc.want)
		}
	}
}

func TestShouldRunAgent(t *testing.T) {
	high := &alert.Alert{Severity: alert.SeverityHigh}
	low := &alert.Alert{Severity: alert.SeverityLow}
	ruleInc := &incident.Incident{Source: "rule"}
	agentInc := &incident.Incident{Source: "agent"}

	if !shouldRunAgent(high, nil) {
		t.Error("high severity alert should trigger agent even without incident")
	}
	if !shouldRunAgent(high, agentInc) {
		t.Error("high severity alert with agent incident should trigger")
	}
	if !shouldRunAgent(low, ruleInc) {
		t.Error("rule-sourced incident should trigger agent path")
	}
	if shouldRunAgent(low, nil) {
		t.Error("low severity without incident should not trigger agent")
	}
}
