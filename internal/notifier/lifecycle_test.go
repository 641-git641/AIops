package notifier

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"aiops/internal/incident"
)

func testIncident() *incident.Incident {
	return &incident.Incident{
		ID: "inc-lc-1", TenantID: "default",
		Name: "order-slow", Severity: "critical", Source: "rule",
		Status: incident.IncidentOpen, Summary: "order latency spike",
	}
}

func TestIncidentLifecycleMessageCreated(t *testing.T) {
	msg := IncidentLifecycleMessage(testIncident(), "created")
	if msg.Title != "【新事故】order-slow" {
		t.Errorf("Title = %q, want 【新事故】order-slow", msg.Title)
	}
	if msg.Action != "created" || msg.IncidentID != "inc-lc-1" {
		t.Errorf("action/incidentID = %q/%q", msg.Action, msg.IncidentID)
	}
	for _, want := range []string{"rule", "critical", "order latency spike", "新事故已创建"} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("Content 缺 %q：%s", want, msg.Content)
		}
	}
}

func TestIncidentLifecycleMessageResolved(t *testing.T) {
	inc := testIncident()
	inc.Status = incident.IncidentResolved
	msg := IncidentLifecycleMessage(inc, "resolved")
	if msg.Title != "【事故已解决】order-slow" {
		t.Errorf("Title = %q, want 【事故已解决】order-slow", msg.Title)
	}
	if !strings.Contains(msg.Content, "事故已解决") {
		t.Errorf("Content 缺「事故已解决」：%s", msg.Content)
	}
}

func TestLifecycleNotifierSendsToEnabledChannels(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(okHandler(rec))
	defer srv.Close()

	svc := NewService("http://localhost:5173")
	svc.Reload([]ChannelConfig{{Name: ChannelHTTP, Enabled: true, Webhook: srv.URL}})

	ln := NewLifecycleNotifier(svc, nil) // audit nil：fail-open 不 panic
	ln.Notify(context.Background(), testIncident(), "created")

	// 追加详情链接（baseURL 拼接）
	if got := rec.get(); len(got) == 0 {
		t.Fatal("webhook 未收到生命周期通知")
	} else {
		var msg Message
		if err := json.Unmarshal(got, &msg); err != nil {
			t.Fatalf("unmarshal err = %v", err)
		}
		if msg.Title != "【新事故】order-slow" || msg.Action != "created" {
			t.Errorf("title/action = %q/%q", msg.Title, msg.Action)
		}
		if msg.URL != "http://localhost:5173/incidents/inc-lc-1" {
			t.Errorf("URL = %q，应为 baseURL 拼接详情链接", msg.URL)
		}
	}
}

func TestLifecycleNotifierNoChannelsNoPanic(t *testing.T) {
	svc := NewService("") // 未配置渠道
	ln := NewLifecycleNotifier(svc, nil)
	ln.Notify(context.Background(), testIncident(), "resolved") // 应静默返回
}
