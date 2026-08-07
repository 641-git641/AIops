package workflow

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParse_NewTriggerTypes(t *testing.T) {
	// interval 触发：with.interval 合法 → 通过
	def, err := Parse([]byte(`workflow:
  id: wf-interval
  triggers:
    - type: interval
      with:
        interval: 5m
  steps:
    - name: ping
      provider: { type: log, with: { message: "tick" } }
`))
	if err != nil {
		t.Fatalf("parse interval workflow: %v", err)
	}
	if len(def.Workflow.Triggers) != 1 || def.Workflow.Triggers[0].Type != "interval" {
		t.Errorf("triggers = %+v", def.Workflow.Triggers)
	}

	// manual 触发：无 with 配置 → 通过
	if _, err := Parse([]byte(`workflow:
  id: wf-manual
  triggers:
    - type: manual
  steps:
    - name: ping
      provider: { type: log }
`)); err != nil {
		t.Fatalf("parse manual workflow: %v", err)
	}

	// interval 缺 with.interval → 拒绝
	if _, err := Parse([]byte(`workflow:
  id: wf-bad
  triggers:
    - type: interval
  steps:
    - name: s
      provider: { type: log }
`)); err == nil {
		t.Error("interval without with.interval should be rejected")
	}

	// 未知名 trigger 类型 → 拒绝
	if _, err := Parse([]byte(`workflow:
  id: wf-x
  triggers:
    - type: cron
  steps:
    - name: s
      provider: { type: log }
`)); err == nil {
		t.Error("unknown trigger type should be rejected")
	}
}

func TestTriggerInterval(t *testing.T) {
	// Go duration 字符串
	tr := Trigger{Type: "interval", With: map[string]any{"interval": "5m"}}
	d, ok := tr.Interval()
	if !ok || d != 5*time.Minute {
		t.Errorf("interval '5m' = %v,%v", d, ok)
	}
	// 纯数字秒
	tr = Trigger{Type: "interval", With: map[string]any{"interval": 30}}
	if d, ok := tr.Interval(); !ok || d != 30*time.Second {
		t.Errorf("interval 30 = %v,%v", d, ok)
	}
	// 非 interval 类型
	if _, ok := (Trigger{Type: "alert"}).Interval(); ok {
		t.Error("alert trigger should not have interval")
	}
	// 缺 with.interval / 非法值
	if _, ok := (Trigger{Type: "interval"}).Interval(); ok {
		t.Error("interval without with.interval should be invalid")
	}
	if _, ok := (Trigger{Type: "interval", With: map[string]any{"interval": "abc"}}).Interval(); ok {
		t.Error("invalid duration should be invalid")
	}
}

// httpStep 扩展：method / headers / bearer auth / 模板渲染。
func TestHTTPStep_MethodHeadersBearer(t *testing.T) {
	var gotMethod, gotAuth, gotHeader, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotHeader = r.Header.Get("X-Trace")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(201)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	a := mkAlert("pay-slow", "pay", "medium", "prometheus")
	s := &Service{http: &http.Client{Timeout: 5 * time.Second}}
	with := map[string]any{
		"url":     srv.URL,
		"method":  "PUT",
		"headers": map[string]any{"X-Trace": "wf-{{.alert.name}}"},
		"auth":    map[string]any{"type": "bearer", "token": "tok-{{.alert.severity}}"},
		"body":    `{"name":"{{.alert.name}}"}`,
	}
	res, err := s.httpStep(context.Background(), with, a)
	if err != nil {
		t.Fatalf("httpStep: %v", err)
	}
	if res["status"] != 201 {
		t.Errorf("status = %v, want 201", res["status"])
	}
	if gotMethod != "PUT" {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotAuth != "Bearer tok-medium" {
		t.Errorf("authorization = %q, want Bearer tok-medium", gotAuth)
	}
	if gotHeader != "wf-pay-slow" {
		t.Errorf("X-Trace = %q, want wf-pay-slow (模板渲染)", gotHeader)
	}
	if gotBody != `{"name":"pay-slow"}` {
		t.Errorf("body = %q, want 模板渲染 body", gotBody)
	}
}

// httpStep basic auth。
func TestHTTPStep_BasicAuth(t *testing.T) {
	var user, pass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ = r.BasicAuth()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := &Service{http: &http.Client{Timeout: 5 * time.Second}}
	_, err := s.httpStep(context.Background(), map[string]any{
		"url":  srv.URL,
		"auth": map[string]any{"type": "basic", "username": "oncall", "password": "secret"},
	}, nil)
	if err != nil {
		t.Fatalf("httpStep basic: %v", err)
	}
	if user != "oncall" || pass != "secret" {
		t.Errorf("basic auth = %q/%q, want oncall/secret", user, pass)
	}
}

// renderTemplate nil alert（interval/manual 无告警运行）：alert 上下文为空，不 panic。
func TestRenderTemplate_NilAlert(t *testing.T) {
	if got := renderTemplate("interval ping", nil); got != "interval ping" {
		t.Errorf("static template with nil alert = %q", got)
	}
	// 引用告警字段 → 空上下文按 missingkey=zero 渲染为 <no value>，不 panic
	if got := renderTemplate("{{.alert.name}}", nil); got != "<no value>" {
		t.Errorf("nil alert template = %q, want <no value>", got)
	}
}

// agentStep nil alert → 明确报错（interval/manual 无告警运行不适用 agent provider）。
func TestAgentStep_NilAlert(t *testing.T) {
	s := &Service{}
	if _, err := s.agentStep(context.Background(), &WorkflowRun{}, nil); err == nil {
		t.Error("agent step with nil alert should error")
	}
}
