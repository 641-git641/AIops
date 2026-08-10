package builtin

import (
	"net/http/httptest"
	"testing"

	"aiops/internal/alert"
)

// parseGeneric 便捷解析。
func parseGeneric(t *testing.T, body string) (*alert.IngestEvent, error) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/webhook/generic", nil)
	events, err := genericProvider{}.Parse([]byte(body), req)
	if err != nil {
		return nil, err
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	return events[0], nil
}

func TestGenericParseMapping(t *testing.T) {
	e, err := parseGeneric(t, `{
		"title":"磁盘使用率过高","severity":"warning","status":"firing","source":"my-script",
		"dedupKey":"node-1-root-disk","labels":{"host":"node-1","disk":"/"},
		"description":"当前使用率 92%"}`)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "磁盘使用率过高" || e.Provider != "generic" || e.ProviderType != "my-script" {
		t.Fatalf("basic mapping wrong: %+v", e)
	}
	// dedupKey 进 Labels（参与指纹）；description 绝不进 Labels
	if e.Labels["dedupKey"] != "node-1-root-disk" {
		t.Fatalf("dedupKey not in labels: %+v", e.Labels)
	}
	if _, ok := e.Labels["description"]; ok {
		t.Fatal("description must not enter Labels（会破坏指纹稳定性）")
	}
	if e.Event["description"] != "当前使用率 92%" {
		t.Fatalf("description missing in Event: %+v", e.Event)
	}
}

func TestGenericSeverityAliases(t *testing.T) {
	cases := map[string]string{
		"p0": "critical", "severe": "critical", "error": "high", "warning": "medium",
		"warn": "medium", "notice": "info", "critical": "critical", "info": "info",
	}
	for in, want := range cases {
		e, err := parseGeneric(t, `{"title":"t","severity":"`+in+`","status":"firing"}`)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		// 归一化发生在 BuildAlert（与全部 provider 一致）
		if got := alert.NormalizeSeverity(e.Severity); got != want {
			t.Errorf("severity %q → %q, want %q", in, got, want)
		}
	}
}

func TestGenericValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"缺 title", `{"severity":"high","status":"firing"}`},
		{"title 空白", `{"title":"  ","severity":"high"}`},
		{"缺 severity", `{"title":"t","status":"firing"}`},
		{"非法 severity", `{"title":"t","severity":"urgent"}`},
		{"非法 status", `{"title":"t","severity":"high","status":"stale"}`},
		{"非法 JSON", `{not json`},
	}
	for _, c := range cases {
		if _, err := parseGeneric(t, c.body); err == nil {
			t.Errorf("%s 必须报错", c.name)
		}
	}
	// status 缺失默认 firing
	e, err := parseGeneric(t, `{"title":"t","severity":"high"}`)
	if err != nil || e.Status != alert.StatusFiring {
		t.Fatalf("default status wrong: %+v, %v", e, err)
	}
	// resolved 合法
	if _, err := parseGeneric(t, `{"title":"t","severity":"high","status":"resolved"}`); err != nil {
		t.Fatal("resolved must pass")
	}
	// source 默认 generic
	e, _ = parseGeneric(t, `{"title":"t","severity":"high"}`)
	if e.ProviderType != "generic" {
		t.Fatalf("default source = %q", e.ProviderType)
	}
}

// TestGenericFingerprintStability 指纹稳定性：同 dedupKey + description 变化 → 指纹不变；
// dedupKey 不同 → 指纹不同（KEEPON 5.2 完成标准"对同一 dedupKey 发送 resolved 后状态正确恢复"）。
func TestGenericFingerprintStability(t *testing.T) {
	fpOf := func(body string) string {
		e, err := parseGeneric(t, body)
		if err != nil {
			t.Fatal(err)
		}
		return e.BuildAlert().Fingerprint
	}
	base := `{"title":"t","severity":"high","source":"s","dedupKey":"k1","labels":{"host":"h"}`
	fp1 := fpOf(base + `,"description":"seq 1"}`)
	fp2 := fpOf(base + `,"description":"seq 2"}`)
	if fp1 != fp2 {
		t.Fatalf("description 变化不得改变指纹: %s != %s", fp1, fp2)
	}
	fp3 := fpOf(`{"title":"t","severity":"high","source":"s","dedupKey":"k2","labels":{"host":"h"}}`)
	if fp1 == fp3 {
		t.Fatal("dedupKey 不同必须指纹不同")
	}
}
