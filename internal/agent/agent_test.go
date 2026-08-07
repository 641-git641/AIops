package agent

import (
	"encoding/json"
	"testing"

	"aiops/internal/alert"
	"aiops/internal/incident"

	"gorm.io/datatypes"
)

func mustJSON(t *testing.T, v any) datatypes.JSON {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestServiceOfFromLabels(t *testing.T) {
	a := &alert.Alert{
		Labels: mustJSON(t, map[string]any{"service": "pay", "cluster": "prod"}),
		Event:  mustJSON(t, map[string]any{"status": "firing"}),
	}
	if got := serviceOf(a); got != "pay" {
		t.Errorf("serviceOf = %q, want pay", got)
	}
}

func TestServiceOfFallbackToEvent(t *testing.T) {
	a := &alert.Alert{
		Labels: mustJSON(t, map[string]any{"severity": "high"}),
		Event:  mustJSON(t, map[string]any{"service_name": "order"}),
	}
	if got := serviceOf(a); got != "order" {
		t.Errorf("serviceOf = %q, want order (event fallback)", got)
	}
}

func TestServiceOfEmpty(t *testing.T) {
	a := &alert.Alert{
		Labels: mustJSON(t, map[string]any{"severity": "high"}),
		Event:  mustJSON(t, map[string]any{"status": "firing"}),
	}
	if got := serviceOf(a); got != "" {
		t.Errorf("serviceOf = %q, want empty", got)
	}
	if got := serviceOf(nil); got != "" {
		t.Errorf("serviceOf(nil) = %q, want empty", got)
	}
}

func TestMonitorAgentWritesResult(t *testing.T) {
	m := NewMonitorAgent()
	a := &alert.Alert{
		ID: "alert-1", Provider: "prometheus", Name: "disk-full",
		Severity: alert.SeverityHigh, Fingerprint: "fp-1",
		Labels: mustJSON(t, map[string]any{"service": "pay"}),
	}
	st := incident.NewState("default", a.ID)
	if err := m.Handle(t.Context(), &Context{State: st, Alert: a}); err != nil {
		t.Fatalf("monitor.Handle error = %v", err)
	}
	if st.Status != incident.StateInvestigating {
		t.Errorf("status = %q, want investigating", st.Status)
	}
	res := st.NodeResult("monitor")
	if res == nil {
		t.Fatal("monitor node result missing")
	}
	if res["service"] != "pay" {
		t.Errorf("monitor result service = %v, want pay", res["service"])
	}
	if res["severity"] != alert.SeverityHigh {
		t.Errorf("monitor result severity = %v, want high", res["severity"])
	}
}
