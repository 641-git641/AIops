package cel

import (
	"testing"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return e
}

func TestEvalBoolComparisons(t *testing.T) {
	e := newTestEngine(t)
	vars := map[string]any{
		"alert":  map[string]any{"severity": "critical", "name": "disk-full", "status": "firing"},
		"labels": map[string]any{"service": "pay", "host": "h1"},
		"event":  map[string]any{},
	}
	cases := []struct {
		expr string
		want bool
	}{
		{"alert.severity == 'critical'", true},
		{"alert.severity == 'high'", false},
		{"labels.service == 'pay'", true},
		{"alert.severity in ['critical','high']", true},
		{"alert.status == 'firing' && labels.host == 'h1'", true},
		{"alert.status == 'resolved'", false},
	}
	for _, tc := range cases {
		got, err := e.EvalBool(tc.expr, vars)
		if err != nil {
			t.Fatalf("EvalBool(%q) error = %v", tc.expr, err)
		}
		if got != tc.want {
			t.Errorf("EvalBool(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestEvalBoolInvalidExpr(t *testing.T) {
	e := newTestEngine(t)
	if _, err := e.EvalBool("alert.severity >=", nil); err == nil {
		t.Fatal("expected compile error for invalid expression")
	}
}

func TestEvalBoolProgramCache(t *testing.T) {
	// 同一表达式应命中缓存且结果稳定
	e := newTestEngine(t)
	vars := map[string]any{"alert": map[string]any{"severity": "high"}}
	for i := 0; i < 3; i++ {
		got, err := e.EvalBool("alert.severity == 'high'", vars)
		if err != nil || !got {
			t.Fatalf("cached eval failed at iter %d: %v", i, err)
		}
	}
}
