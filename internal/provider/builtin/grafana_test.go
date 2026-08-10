package builtin

import (
	"testing"
	"time"
)

// grafanaPayload Grafana Unified Alerting webhook 示例负载。
const grafanaPayload = `{
  "receiver":"aiops","status":"firing","version":"1",
  "alerts":[
    {
      "status":"firing",
      "labels":{"alertname":"High memory usage","grafana_folder":"Production","severity":"critical","service":"pay"},
      "annotations":{"summary":"Memory usage above 80%"},
      "startsAt":"2026-08-09T10:00:00Z","endsAt":"0001-01-01T00:00:00Z",
      "generatorURL":"http://grafana.example.com/graph/abc","fingerprint":"c995d40600c35b35",
      "values":{"B":82.3}
    }
  ],
  "groupLabels":{"alertname":"High memory usage"},
  "commonLabels":{"grafana_folder":"Production"},
  "externalURL":"http://grafana.example.com",
  "title":"[FIRING:1] High memory usage","state":"alerting"
}`

func TestGrafanaParse(t *testing.T) {
	p := &GrafanaProvider{}
	events, err := p.Parse([]byte(grafanaPayload), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	e := events[0]
	if e.Name != "Memory usage above 80%" {
		t.Fatalf("name = %q（应取 annotations.summary）", e.Name)
	}
	if e.Severity != "critical" {
		t.Fatalf("severity = %q", e.Severity)
	}
	if e.Status != "firing" {
		t.Fatalf("status = %q", e.Status)
	}
	if e.Provider != "grafana" || e.ProviderType != "grafana" {
		t.Fatalf("provider = %q/%q", e.Provider, e.ProviderType)
	}
	if e.Labels["service"] != "pay" || e.Labels["grafana_folder"] != "Production" {
		t.Fatalf("labels = %v", e.Labels)
	}
	// Grafana 特有字段进 Event
	ev := e.Event
	if ev["generatorURL"] != "http://grafana.example.com/graph/abc" || ev["fingerprint"] != "c995d40600c35b35" {
		t.Fatalf("event missing grafana fields: %v", ev)
	}
	if _, ok := ev["values"].(map[string]float64); !ok {
		t.Fatalf("event values = %v", ev["values"])
	}
	if e.StartedAt == nil || !e.StartedAt.Equal(time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("startedAt = %v", e.StartedAt)
	}
}

func TestGrafanaParseFallbacks(t *testing.T) {
	p := &GrafanaProvider{}
	// 仅 labels.alertname、无 startsAt、无 per-alert status → 顶层 status 兜底
	events, err := p.Parse([]byte(`{"status":"firing","alerts":[{"labels":{"alertname":"disk-full"}}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	e := events[0]
	if e.Name != "disk-full" {
		t.Fatalf("name = %q", e.Name)
	}
	if e.Severity != "high" {
		t.Fatalf("severity = %q（应默认 high）", e.Severity)
	}
	if e.Status != "firing" {
		t.Fatalf("status = %q（应顶层兜底）", e.Status)
	}
	if e.StartedAt == nil || time.Since(*e.StartedAt) > 5*time.Second {
		t.Fatalf("startedAt 未兜底 now: %v", e.StartedAt)
	}
	// 全空兜底名
	events2, err := p.Parse([]byte(`{"alerts":[{}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if events2[0].Name != "grafana-alert" {
		t.Fatalf("fallback name = %q", events2[0].Name)
	}
}

func TestGrafanaParseResolved(t *testing.T) {
	p := &GrafanaProvider{}
	events, err := p.Parse([]byte(`{"status":"resolved","alerts":[{"status":"resolved","labels":{"alertname":"x"}}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Status != "resolved" {
		t.Fatalf("status = %q", events[0].Status)
	}
}

func TestGrafanaParseInvalid(t *testing.T) {
	p := &GrafanaProvider{}
	if _, err := p.Parse([]byte("{not-json"), nil); err == nil {
		t.Fatal("want parse error")
	}
	if _, err := p.Parse([]byte(""), nil); err == nil {
		t.Fatal("want parse error on empty body")
	}
	// 空 alerts 数组 → 空结果不报错
	events, err := p.Parse([]byte(`{"status":"firing","alerts":[]}`), nil)
	if err != nil || len(events) != 0 {
		t.Fatalf("empty alerts: %v %v", events, err)
	}
}
