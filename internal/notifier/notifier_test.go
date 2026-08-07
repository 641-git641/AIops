package notifier

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recorder 捕获最后一次请求体（各渠道格式断言用）。
type recorder struct {
	mu   sync.Mutex
	body []byte
}

func (r *recorder) get() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body
}

func okHandler(rec *recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		rec.mu.Lock()
		rec.body = b
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func testMsg() Message {
	return Message{
		Title: "告警：pay-slow", Content: "**状态**：⏳ 等待审批",
		Severity: "critical", IncidentID: "inc-1", CorrelationID: "corr-1",
		URL: "http://localhost:5173/incidents/inc-1", Action: "pending_approval",
	}
}

func TestDingTalkSendsMarkdown(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(okHandler(rec))
	defer srv.Close()

	err := NewDingTalk(srv.URL, srv.Client()).Send(context.Background(), testMsg())
	if err != nil {
		t.Fatalf("Send err = %v", err)
	}
	var got struct {
		MsgType  string `json:"msgtype"`
		Markdown struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"markdown"`
	}
	if err := json.Unmarshal(rec.get(), &got); err != nil {
		t.Fatalf("unmarshal err = %v", err)
	}
	if got.MsgType != "markdown" {
		t.Errorf("msgtype = %q, want markdown", got.MsgType)
	}
	if got.Markdown.Title != "告警：pay-slow" {
		t.Errorf("title = %q", got.Markdown.Title)
	}
	for _, want := range []string{"等待审批", "inc-1", "corr-1", "incidents/inc-1"} {
		if !strings.Contains(got.Markdown.Text, want) {
			t.Errorf("text 缺少 %q: %s", want, got.Markdown.Text)
		}
	}
}

func TestFeishuSendsCard(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(okHandler(rec))
	defer srv.Close()

	if err := NewFeishu(srv.URL, srv.Client()).Send(context.Background(), testMsg()); err != nil {
		t.Fatalf("Send err = %v", err)
	}
	var got struct {
		MsgType string `json:"msg_type"`
		Card    struct {
			Header struct {
				Title struct {
					Content string `json:"content"`
				} `json:"title"`
				Template string `json:"template"`
			} `json:"header"`
			Elements []struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"card"`
	}
	if err := json.Unmarshal(rec.get(), &got); err != nil {
		t.Fatalf("unmarshal err = %v", err)
	}
	if got.MsgType != "interactive" {
		t.Errorf("msg_type = %q, want interactive", got.MsgType)
	}
	if got.Card.Header.Template != "red" {
		t.Errorf("template = %q, want red (critical)", got.Card.Header.Template)
	}
	if got.Card.Header.Title.Content != "告警：pay-slow" {
		t.Errorf("card title = %q", got.Card.Header.Title.Content)
	}
	if len(got.Card.Elements) != 1 || got.Card.Elements[0].Tag != "markdown" {
		t.Fatalf("elements 异常: %+v", got.Card.Elements)
	}
	if !strings.Contains(got.Card.Elements[0].Content, "等待审批") {
		t.Errorf("card 正文缺少内容: %s", got.Card.Elements[0].Content)
	}
}

func TestWecomSendsMarkdown(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(okHandler(rec))
	defer srv.Close()

	if err := NewWecom(srv.URL, srv.Client()).Send(context.Background(), testMsg()); err != nil {
		t.Fatalf("Send err = %v", err)
	}
	var got struct {
		MsgType  string `json:"msgtype"`
		Markdown struct {
			Content string `json:"content"`
		} `json:"markdown"`
	}
	if err := json.Unmarshal(rec.get(), &got); err != nil {
		t.Fatalf("unmarshal err = %v", err)
	}
	if got.MsgType != "markdown" {
		t.Errorf("msgtype = %q", got.MsgType)
	}
	if !strings.Contains(got.Markdown.Content, "告警：pay-slow") {
		t.Errorf("企微正文应含标题: %s", got.Markdown.Content)
	}
}

func TestHTTPSendsMessage(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(okHandler(rec))
	defer srv.Close()

	msg := testMsg()
	if err := NewHTTP(srv.URL, srv.Client()).Send(context.Background(), msg); err != nil {
		t.Fatalf("Send err = %v", err)
	}
	var got Message
	if err := json.Unmarshal(rec.get(), &got); err != nil {
		t.Fatalf("unmarshal err = %v", err)
	}
	if got.Title != msg.Title || got.Content != msg.Content ||
		got.IncidentID != msg.IncidentID || got.Action != msg.Action {
		t.Errorf("通用 webhook 未原样转发 Message: %+v", got)
	}
}

func TestServiceNotifyAllChannels(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(okHandler(rec))
	defer srv.Close()

	svc := NewService("http://localhost:5173")
	svc.Add(NewDingTalk(srv.URL, srv.Client()))
	svc.Add(NewHTTP(srv.URL, srv.Client()))

	results := svc.Notify(context.Background(), Message{Title: "t", Content: "c", IncidentID: "inc-1"})
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if !svc.AllOK(results) {
		t.Errorf("AllOK = false, results = %+v", results)
	}
	if !strings.HasPrefix(svc.Enabled()[0], "dingtalk") {
		t.Errorf("Enabled = %v, want dingtalk 在前", svc.Enabled())
	}
}

func TestServiceNotifyFailureAggregates(t *testing.T) {
	// 一个渠道 500 失败，另一个成功：结果聚合不中断
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()
	rec := &recorder{}
	good := httptest.NewServer(okHandler(rec))
	defer good.Close()

	svc := NewService("")
	svc.Add(NewDingTalk(bad.URL, bad.Client()))
	svc.Add(NewHTTP(good.URL, good.Client()))

	results := svc.Notify(context.Background(), Message{Title: "t", Content: "c"})
	if svc.AllOK(results) {
		t.Fatalf("AllOK = true, want false: %+v", results)
	}
	for _, r := range results {
		if !r.OK && r.Error == "" {
			t.Errorf("失败渠道应带 Error: %+v", r)
		}
		if r.Channel == "http" && !r.OK {
			t.Errorf("good 渠道不应失败: %+v", r)
		}
	}
}

func TestServiceNotifyNoChannels(t *testing.T) {
	svc := NewService("")
	if got := svc.Notify(context.Background(), Message{Title: "t"}); len(got) != 0 {
		t.Errorf("无渠道时应返回空结果: %+v", got)
	}
	if svc.Len() != 0 {
		t.Errorf("Len = %d, want 0", svc.Len())
	}
}

func TestNewSkipsEmptyWebhooks(t *testing.T) {
	svc := New("dingtalk,feishu,http", "", "https://feishu/x", "", "https://http/y", "")
	got := svc.Enabled() // dingtalk 无 webhook → 跳过
	if len(got) != 2 || got[0] != "feishu" || got[1] != "http" {
		t.Errorf("Enabled = %v, want [feishu http]", got)
	}
}

func TestServiceAutoBuildIncidentURL(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(okHandler(rec))
	defer srv.Close()

	svc := NewService("http://localhost:5173")
	svc.Add(NewHTTP(srv.URL, srv.Client()))
	svc.Notify(context.Background(), Message{Title: "t", Content: "c", IncidentID: "inc-9"})

	var got Message
	if err := json.Unmarshal(rec.get(), &got); err != nil {
		t.Fatalf("unmarshal err = %v", err)
	}
	if want := "http://localhost:5173/incidents/inc-9"; got.URL != want {
		t.Errorf("URL = %q, want %q", got.URL, want)
	}
}

func TestRedactQuery(t *testing.T) {
	got := redact(`Post "https://oapi.dingtalk.com/robot/send?access_token=abc123": dial tcp: no route`)
	if strings.Contains(got, "abc123") {
		t.Errorf("redact 未抹除 token: %s", got)
	}
	if !strings.Contains(got, "?***") {
		t.Errorf("redact 应保留 ?*** 占位: %s", got)
	}
}

func TestFeishuColorMapping(t *testing.T) {
	cases := map[string]string{
		"critical": "red", "high": "orange", "medium": "yellow", "low": "blue", "": "blue",
	}
	for sev, want := range cases {
		if got := feishuColor(sev); got != want {
			t.Errorf("feishuColor(%q) = %q, want %q", sev, got, want)
		}
	}
}
