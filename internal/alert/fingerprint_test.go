package alert

import (
	"testing"
)

func TestFingerprintIgnoresVolatileLabels(t *testing.T) {
	base := map[string]any{"service": "pay", "severity": "high"}
	a := Fingerprint("prometheus", "disk-full", base)
	b := Fingerprint("prometheus", "disk-full", map[string]any{
		"service": "pay", "severity": "high",
		"instance": "192.168.1.1", "timestamp": "2026-08-07T10:00:00Z",
	})
	if a != b {
		t.Fatal("volatile labels should not affect fingerprint")
	}
}

func TestFingerprintSensitiveToStableLabels(t *testing.T) {
	a := Fingerprint("prometheus", "disk-full", map[string]any{"service": "pay"})
	b := Fingerprint("prometheus", "disk-full", map[string]any{"service": "order"})
	if a == b {
		t.Fatal("stable label difference should change fingerprint")
	}
}

func TestFingerprintOrderIndependent(t *testing.T) {
	a := Fingerprint("prometheus", "disk-full", map[string]any{"service": "pay", "cluster": "prod"})
	b := Fingerprint("prometheus", "disk-full", map[string]any{"cluster": "prod", "service": "pay"})
	if a != b {
		t.Fatal("fingerprint must be independent of map iteration order")
	}
}

func TestBuildAlertDefaults(t *testing.T) {
	e := &IngestEvent{Provider: "prometheus", Name: "cpu-high", Severity: "error"}
	a := e.BuildAlert()
	if a.ID == "" || a.Fingerprint == "" || a.AlertHash == "" {
		t.Fatalf("alert missing computed fields: %+v", a)
	}
	// "error" 应归一化为 high
	if a.Severity != SeverityHigh {
		t.Errorf("severity = %q, want high", a.Severity)
	}
	if a.Status != StatusFiring {
		t.Errorf("status = %q, want firing", a.Status)
	}
	if a.Event == nil || len(a.Event) == 0 {
		t.Error("event snapshot should be populated")
	}
}
