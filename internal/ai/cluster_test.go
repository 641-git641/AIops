package ai

import (
	"encoding/json"
	"testing"
	"time"

	"aiops/internal/alert"
)

func mkAlert(t *testing.T, id, name, svc, sev string, at time.Time) alert.Alert {
	t.Helper()
	labels := map[string]any{"service": svc}
	b, _ := json.Marshal(labels)
	return alert.Alert{ID: id, Name: name, Severity: sev, Status: alert.StatusFiring, Labels: b, CreatedAt: at}
}

func TestClusterAlerts_GroupByService(t *testing.T) {
	base := time.Now().UTC()
	alerts := []alert.Alert{
		mkAlert(t, "a1", "pay-slow", "pay", alert.SeverityHigh, base),
		mkAlert(t, "a2", "pay-timeout", "pay", alert.SeverityCritical, base.Add(time.Minute)),
		mkAlert(t, "a3", "order-down", "order", alert.SeverityCritical, base.Add(2*time.Minute)),
	}
	clusters := ClusterAlerts(alerts, 10*time.Minute)
	if len(clusters) != 2 {
		t.Fatalf("want 2 clusters (pay/order), got %d", len(clusters))
	}
	bySvc := map[string]*Cluster{}
	for i := range clusters {
		bySvc[clusters[i].Service] = &clusters[i]
	}
	pay := bySvc["pay"]
	if pay == nil || pay.Count != 2 {
		t.Fatalf("pay cluster count = %v, want 2", pay)
	}
	if pay.Severity != alert.SeverityCritical {
		t.Errorf("pay cluster severity = %s, want critical (取最高)", pay.Severity)
	}
	if len(pay.Names) != 2 || pay.Names[0] != "pay-slow" || pay.Names[1] != "pay-timeout" {
		t.Errorf("pay cluster names = %v, want sorted [pay-slow pay-timeout]", pay.Names)
	}
}

func TestClusterAlerts_GapSplits(t *testing.T) {
	base := time.Now().UTC()
	alerts := []alert.Alert{
		mkAlert(t, "a1", "pay-slow", "pay", alert.SeverityHigh, base),
		mkAlert(t, "a2", "pay-slow", "pay", alert.SeverityHigh, base.Add(30*time.Minute)), // 超过 10min 窗口 → 断开
	}
	clusters := ClusterAlerts(alerts, 10*time.Minute)
	if len(clusters) != 2 {
		t.Fatalf("gap > window 应断开为 2 簇, got %d", len(clusters))
	}
}

func TestClusterAlerts_KeyStableAcrossRuns(t *testing.T) {
	base := time.Now().UTC()
	a := []alert.Alert{
		mkAlert(t, "a1", "pay-slow", "pay", alert.SeverityCritical, base),
		mkAlert(t, "a2", "pay-timeout", "pay", alert.SeverityCritical, base.Add(time.Minute)),
	}
	c1 := ClusterAlerts(a, 10*time.Minute)

	// 时间不同但构成相同 → key 应一致（与时间无关）
	b := []alert.Alert{
		mkAlert(t, "b1", "pay-slow", "pay", alert.SeverityCritical, base.Add(time.Hour)),
		mkAlert(t, "b2", "pay-timeout", "pay", alert.SeverityCritical, base.Add(time.Hour+time.Minute)),
	}
	c2 := ClusterAlerts(b, 10*time.Minute)
	if c1[0].Key != c2[0].Key {
		t.Errorf("key 不稳定：%s != %s", c1[0].Key, c2[0].Key)
	}
	// 新增告警名 → key 变化（构成变化）
	c3 := ClusterAlerts(append(b, mkAlert(t, "b3", "pay-restart", "pay", alert.SeverityCritical, base.Add(time.Hour+2*time.Minute))), 10*time.Minute)
	if c3[0].Key == c1[0].Key {
		t.Error("新增告警名后 key 不应相同")
	}
}

func TestClusterAlerts_UnknownService(t *testing.T) {
	base := time.Now().UTC()
	labels := map[string]any{}
	b, _ := json.Marshal(labels)
	a := alert.Alert{ID: "a1", Name: "cpu-high", Severity: alert.SeverityHigh, Labels: b, CreatedAt: base}
	clusters := ClusterAlerts([]alert.Alert{a}, 10*time.Minute)
	if clusters[0].Service != "unknown" {
		t.Errorf("无 service 标签应归 unknown, got %s", clusters[0].Service)
	}
}

func TestSummarize(t *testing.T) {
	base := time.Now().UTC()
	c := &Cluster{
		Service: "pay", Names: []string{"pay-slow", "pay-timeout"},
		Severity: alert.SeverityCritical, Count: 5,
		StartedAt: base, LastSeenAt: base.Add(3 * time.Minute),
	}
	name, summary := Summarize(c)
	if name == "" || summary == "" {
		t.Fatal("name/summary 不应为空")
	}
	if name != "严重 pay（5 条关联告警）" {
		t.Errorf("name = %q, want 严重 pay（5 条关联告警）", name)
	}
}
