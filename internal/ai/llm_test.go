package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("", "", "")
	if c.baseURL != "https://api.deepseek.com" {
		t.Errorf("baseURL = %q, want deepseek default", c.baseURL)
	}
	if c.model != "deepseek-chat" {
		t.Errorf("model = %q, want deepseek-chat", c.model)
	}
	if got := c.chatURL(); got != "https://api.deepseek.com/chat/completions" {
		t.Errorf("chatURL = %q", got)
	}
}

func TestChatURLAlreadySuffixed(t *testing.T) {
	c := NewClient("https://api.deepseek.com/chat/completions", "", "")
	if got := c.chatURL(); got != "https://api.deepseek.com/chat/completions" {
		t.Errorf("chatURL = %q, want unchanged", got)
	}
}

func TestParseRCAOutput(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantRC  string
		wantErr bool
	}{
		{name: "plain json", in: `{"rootCause":"db 主从延迟","rationale":"x","suggestedActions":["回滚","切换"]}`, wantRC: "db 主从延迟"},
		{name: "fenced json", in: "```json\n{\"rootCause\":\"cpu 压满\",\"suggestedActions\":[\"扩容\"]}\n```", wantRC: "cpu 压满"},
		{name: "surrounding text", in: "下面是结果：{\"rootCause\":\"网络抖动\",\"rationale\":\"y\",\"suggestedActions\":[\"检查网络\"]} 完", wantErr: true},
		{name: "missing rootCause", in: `{"rationale":"y"}`, wantErr: true},
		{name: "invalid json", in: `not json`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := parseRCAOutput(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse error = %v", err)
			}
			if out.RootCause != tc.wantRC {
				t.Errorf("rootCause = %q, want %q", out.RootCause, tc.wantRC)
			}
		})
	}
}

// mockChatServer 返回一个伪造的 OpenAI 兼容端点，捕获请求供断言。
func mockChatServer(t *testing.T, status int, content string) (*httptest.Server, *chatRequest) {
	t.Helper()
	var got chatRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s, want /chat/completions", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"message":"boom"}}`)
			return
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
	}))
	return ts, &got
}

func TestEnhanceRCA(t *testing.T) {
	ts, req := mockChatServer(t, http.StatusOK,
		"```json\n{\"rootCause\":\"order 数据库主从延迟\",\"rationale\":\"近期变更 v1.2.3 导致慢查询\",\"suggestedActions\":[\"回滚发布\",\"主从切换\"]}\n```")
	defer ts.Close()

	c := NewClient(ts.URL, "test-key", "deepseek-chat")
	c.http = ts.Client()

	out, err := c.EnhanceRCA(t.Context(), RCAInput{
		Service: "order", AlertName: "order-slow", Severity: "medium",
		Summary: "order latency spike", Labels: map[string]string{"pod": "order-1"},
		RootCause: "recent_change", Confidence: 0.54,
		Evidence: []string{"受影响服务存在近期变更：v1.2.3"}, Changes: []string{"v1.2.3"},
		Paths: []string{"order → db"},
	})
	if err != nil {
		t.Fatalf("EnhanceRCA error = %v", err)
	}
	if out.RootCause != "order 数据库主从延迟" {
		t.Errorf("rootCause = %q", out.RootCause)
	}
	if len(out.SuggestedActions) != 2 {
		t.Errorf("suggestedActions len = %d, want 2", len(out.SuggestedActions))
	}

	// 请求断言：模型、系统提示、用户输入包含关键事实
	if req.Model != "deepseek-chat" {
		t.Errorf("model = %q, want deepseek-chat", req.Model)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v, want [system, user]", req.Messages)
	}
	if !strings.Contains(req.Messages[0].Content, "JSON") {
		t.Errorf("system prompt 应要求 JSON 输出")
	}
	for _, want := range []string{"order-slow", "order latency spike", "recent_change", "0.54", "order → db"} {
		if !strings.Contains(req.Messages[1].Content, want) {
			t.Errorf("user prompt 缺少 %q，got:\n%s", want, req.Messages[1].Content)
		}
	}
}

func TestEnhanceRCAUnauthorized(t *testing.T) {
	ts, _ := mockChatServer(t, http.StatusUnauthorized, "")
	defer ts.Close()
	c := NewClient(ts.URL, "bad-key", "deepseek-chat")
	c.http = ts.Client()

	if _, err := c.EnhanceRCA(t.Context(), RCAInput{AlertName: "x"}); err == nil {
		t.Fatal("want error on 401, got nil")
	}
}

func TestCompleteSkipsAuthHeaderWhenNoKey(t *testing.T) {
	var authHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "", "m")
	c.http = ts.Client()
	if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Complete error = %v", err)
	}
	if authHeader != "" {
		t.Errorf("Authorization = %q, want empty when apiKey 未配置", authHeader)
	}
}

func TestParseClusterOutput(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "plain json", in: `{"summary":"order 服务连接池耗尽"}`},
		{name: "fenced json", in: "```json\n{\"summary\":\"cpu 压满，建议扩容\"}\n```", want: "cpu 压满，建议扩容"},
		{name: "missing summary", in: `{"other":"x"}`, wantErr: true},
		{name: "invalid json", in: `not json`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := parseClusterOutput(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse error = %v", err)
			}
			if tc.want != "" && out.Summary != tc.want {
				t.Errorf("summary = %q, want %q", out.Summary, tc.want)
			}
		})
	}
}

func TestEnhanceCluster(t *testing.T) {
	ts, req := mockChatServer(t, http.StatusOK,
		"```json\n{\"summary\":\"order 服务并发告警集中爆发，疑似连接池耗尽，建议扩容并核查近期变更（mock LLM）\"}\n```")
	defer ts.Close()

	c := NewClient(ts.URL, "test-key", "deepseek-chat")
	c.http = ts.Client()

	out, err := c.EnhanceCluster(t.Context(), ClusterInput{
		Service: "order", Severity: "critical", Names: []string{"conn-pool-exhausted", "cpu-high"}, Count: 5,
	})
	if err != nil {
		t.Fatalf("EnhanceCluster error = %v", err)
	}
	if !strings.Contains(out.Summary, "order") {
		t.Errorf("summary = %q, want 包含服务名", out.Summary)
	}
	if req.Stream {
		t.Errorf("stream = %v, want false（默认非流式）", req.Stream)
	}
	// 用户输入应包含告警簇事实，便于模型概括
	user := req.Messages[1].Content
	for _, want := range []string{"order", "critical", "5 条关联告警", "conn-pool-exhausted"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt 缺少 %q，got:\n%s", want, user)
		}
	}
}

// chunkRunes 按 rune 数切分字符串（避免切到 UTF-8 字节中间导致 JSON 转义损坏）。
func chunkRunes(s string, n int) []string {
	runes := []rune(s)
	var out []string
	for i := 0; i < len(runes); i += n {
		end := i + n
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// sseDataLine 构造一条 SSE `data:` 事件（delta.content 用 json.Marshal，非 ASCII 转义为 \uXXXX，保证可解析）。
func sseDataLine(delta string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": delta}}},
	})
	return string(b)
}

// sseMockServer 返回一个逐 delta 输出的 SSE 端点（模拟流式响应）。
func sseMockServer(t *testing.T, chunks []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
}

func TestCompleteStreamAccumulatesDeltas(t *testing.T) {
	// 把目标 JSON 切成多个 delta 分块，验证流式累计后与整体一致
	target := "```json\n{\"rootCause\":\"连接池耗尽\",\"suggestedActions\":[\"重启\"]}\n```"
	var chunks []string
	for _, c := range chunkRunes(target, 8) {
		chunks = append(chunks, sseDataLine(c))
	}
	ts := sseMockServer(t, chunks)
	defer ts.Close()

	c := NewClient(ts.URL, "k", "m")
	c.http = ts.Client()
	c.SetStream(true)

	out, err := c.CompleteStream(t.Context(), "sys", "user")
	if err != nil {
		t.Fatalf("CompleteStream error = %v", err)
	}
	if out != strings.TrimSpace(target) {
		t.Errorf("stream 累计 = %q, want %q", out, strings.TrimSpace(target))
	}
}

func TestEnhanceRCAStreaming(t *testing.T) {
	content := "```json\n{\"rootCause\":\"db 主从延迟\",\"rationale\":\"流式验证\",\"suggestedActions\":[\"主从切换\"]}\n```"
	var chunks []string
	for _, c := range chunkRunes(content, 8) {
		chunks = append(chunks, sseDataLine(c))
	}
	var gotStream bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotStream = req.Stream
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "k", "m")
	c.http = ts.Client()
	c.SetStream(true)

	out, err := c.EnhanceRCA(t.Context(), RCAInput{AlertName: "x"})
	if err != nil {
		t.Fatalf("EnhanceRCA stream error = %v", err)
	}
	if out.RootCause != "db 主从延迟" {
		t.Errorf("rootCause = %q", out.RootCause)
	}
	if !gotStream {
		t.Error("stream 模式下请求体 stream 应为 true")
	}
}

func TestCompleteStreamServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":{"message":"upstream down"}}`)
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "k", "m")
	c.http = ts.Client()
	c.SetStream(true)
	if _, err := c.CompleteStream(t.Context(), "s", "u"); err == nil {
		t.Fatal("want error on 502, got nil")
	}
}

func TestEnhanceClusterTimeout(t *testing.T) {
	// 服务端故意慢于客户端超时 → 应返回错误（超时可控），fail-open 由调用方兜底
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"{\"summary\":\"慢响应\"}"}}]}`)
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "k", "m")
	c.http = ts.Client()
	c.SetTimeout(50 * time.Millisecond) // 客户端 50ms，服务端 300ms → 超时（覆盖 http.Client.Timeout）

	if _, err := c.EnhanceCluster(t.Context(), ClusterInput{Service: "s"}); err == nil {
		t.Fatal("want timeout error, got nil")
	}
}

func TestSetTimeoutIgnoresNonPositive(t *testing.T) {
	c := NewClient("", "", "")
	c.SetTimeout(0)
	if got := c.http.Timeout; got != 15*time.Second {
		t.Errorf("http.Timeout = %v, want 保留默认 15s", got)
	}
	c.SetTimeout(30 * time.Second)
	if got := c.http.Timeout; got != 30*time.Second {
		t.Errorf("http.Timeout = %v, want 30s", got)
	}
}

func TestCompleteStreamTimeoutMidStream(t *testing.T) {
	// 流式累计中途超时：先收到若干 delta，随后服务端停滞超过客户端超时 → 应返回错误
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			sseDataLine("part1"),
			sseDataLine("part2"),
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond) // 服务端停滞，超过客户端 50ms 超时
		fmt.Fprintf(w, "data: %s\n\n", sseDataLine("late"))
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "k", "m")
	c.http = ts.Client()
	c.SetStream(true)
	c.SetTimeout(50 * time.Millisecond)

	if _, err := c.CompleteStream(t.Context(), "s", "u"); err == nil {
		t.Fatal("want timeout error mid-stream, got nil")
	}
}
