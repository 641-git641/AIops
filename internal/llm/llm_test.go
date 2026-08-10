package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// reqCapture 捕获请求体（含流式字段与工具定义）。
type reqCapture struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
}

// mockServer 伪造 OpenAI 兼容端点：捕获请求 + 按 handler 返回。
func mockServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, cap *reqCapture)) (*httptest.Server, *reqCapture) {
	t.Helper()
	var cap reqCapture
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			t.Errorf("method/path = %s %s", r.Method, r.URL.Path)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&cap); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		handler(w, r, &cap)
	}))
	return ts, &cap
}

func newClient(ts *httptest.Server) *Client {
	c := NewClient(ts.URL, "test-key", "mock-model")
	c.http = ts.Client()
	return c
}

// ---- 非流式 ----

func TestChatCompleteWithTools(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"severity":{"type":"string"}}}`)
	ts, cap := mockServer(t, func(w http.ResponseWriter, _ *http.Request, _ *reqCapture) {
		fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"tool_calls","message":{
			"role":"assistant","content":null,
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_alerts","arguments":"{\"severity\":\"critical\"}"}}]}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	})
	defer ts.Close()
	c := newClient(ts)

	comp, err := c.Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "列出 critical 告警"}},
		Tools:    []Tool{{Type: "function", Function: ToolFunction{Name: "list_alerts", Description: "d", Parameters: schema}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 请求体断言：角色数组、tools 原样、temperature
	if cap.Model != "mock-model" || len(cap.Messages) != 1 || cap.Messages[0].Role != RoleUser {
		t.Fatalf("request = %+v", cap)
	}
	if len(cap.Tools) != 1 || cap.Tools[0].Function.Name != "list_alerts" ||
		!strings.Contains(string(cap.Tools[0].Function.Parameters), "severity") {
		t.Fatalf("tools = %+v", cap.Tools)
	}
	// 响应断言：tool_calls + finish_reason + usage
	if comp.FinishReason != FinishToolCalls {
		t.Errorf("finish = %q, want tool_calls", comp.FinishReason)
	}
	if len(comp.ToolCalls) != 1 || comp.ToolCalls[0].Function.Name != "list_alerts" ||
		!strings.Contains(comp.ToolCalls[0].Function.Arguments, "critical") {
		t.Fatalf("toolCalls = %+v", comp.ToolCalls)
	}
	if comp.Usage == nil || comp.Usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v", comp.Usage)
	}
}

func TestChatCompleteContentNullSafe(t *testing.T) {
	ts, _ := mockServer(t, func(w http.ResponseWriter, _ *http.Request, _ *reqCapture) {
		fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"你好"}}]}`)
	})
	defer ts.Close()
	comp, err := newClient(ts).Chat(t.Context(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Content != "你好" || comp.FinishReason != FinishStop {
		t.Fatalf("comp = %+v", comp)
	}
}

// ---- 流式 ----

// writeSSE 逐行写 SSE 事件。
func writeSSE(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestChatStreamContentChunks(t *testing.T) {
	ts, _ := mockServer(t, func(w http.ResponseWriter, _ *http.Request, _ *reqCapture) {
		// 中文 rune 分片累计
		writeSSE(w,
			`{"choices":[{"delta":{"content":"告警"}}]}`,
			`{"choices":[{"delta":{"content":" 数量"}}]}`,
			`{"choices":[{"delta":{}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	})
	defer ts.Close()
	c := newClient(ts)
	c.SetStream(true)

	var deltas []string
	comp, err := c.Chat(t.Context(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "q"}}}, func(d Delta) {
		deltas = append(deltas, d.Content)
	})
	if err != nil {
		t.Fatal(err)
	}
	if comp.Content != "告警 数量" || comp.FinishReason != FinishStop {
		t.Fatalf("comp = %+v", comp)
	}
	if comp.Usage == nil || comp.Usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v", comp.Usage)
	}
	if len(deltas) != 2 || strings.Join(deltas, "") != "告警 数量" {
		t.Fatalf("deltas = %v", deltas)
	}
}

func TestChatStreamToolArgsFragments(t *testing.T) {
	// 同一 index 的 arguments 跨 3 chunk 碎片拼接
	ts, _ := mockServer(t, func(w http.ResponseWriter, _ *http.Request, _ *reqCapture) {
		writeSSE(w,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"list_alerts","arguments":"{\"severity\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"critical\",\"status\":\"firing\""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	})
	defer ts.Close()
	c := newClient(ts)
	c.SetStream(true)

	comp, err := c.Chat(t.Context(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "q"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(comp.ToolCalls) != 1 {
		t.Fatalf("toolCalls = %+v", comp.ToolCalls)
	}
	tc := comp.ToolCalls[0]
	if tc.ID != "call_9" || tc.Function.Name != "list_alerts" {
		t.Fatalf("tc = %+v", tc)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("args 未拼成合法 JSON: %q (%v)", tc.Function.Arguments, err)
	}
	if args["severity"] != "critical" || args["status"] != "firing" {
		t.Fatalf("args = %v", args)
	}
}

func TestChatStreamParallelToolCalls(t *testing.T) {
	// index 0/1 交替出现（并行多调用）
	ts, _ := mockServer(t, func(w http.ResponseWriter, _ *http.Request, _ *reqCapture) {
		writeSSE(w,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"list_alerts","arguments":"{\"limit\":5}"}},{"index":1,"id":"c1","function":{"name":"get_platform_overview","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":""}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	})
	defer ts.Close()
	c := newClient(ts)
	c.SetStream(true)

	comp, err := c.Chat(t.Context(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "q"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(comp.ToolCalls) != 2 || comp.ToolCalls[0].Function.Name != "list_alerts" ||
		comp.ToolCalls[1].Function.Name != "get_platform_overview" {
		t.Fatalf("toolCalls = %+v", comp.ToolCalls)
	}
}

// ---- 错误与边界 ----

func TestChatStatusErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusBadGateway} {
		ts, _ := mockServer(t, func(w http.ResponseWriter, _ *http.Request, _ *reqCapture) {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
		})
		_, err := newClient(ts).Chat(t.Context(), ChatRequest{}, nil)
		ts.Close()
		if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), fmt.Sprint(status)) {
			t.Errorf("status %d: err = %v", status, err)
		}
	}
}

func TestChatStreamTimeoutMidStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		fl.Flush()
		time.Sleep(5 * time.Second) // 超过客户端超时
	}))
	defer ts.Close()
	c := newClient(ts)
	c.SetTimeout(200 * time.Millisecond)
	c.SetStream(true)

	start := time.Now()
	_, err := c.Chat(t.Context(), ChatRequest{}, nil)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout 未生效")
	}
}

func TestChatStreamCtxCancel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		fl.Flush()
		time.Sleep(2 * time.Second) // ctx 取消后客户端提前断开读取
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n")
	}))
	defer ts.Close()
	c := newClient(ts)
	c.SetStream(true)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := c.Chat(ctx, ChatRequest{}, nil)
	if err == nil {
		t.Fatal("want ctx cancel error")
	}
}

func TestChatNoAPIKeyHeader(t *testing.T) {
	var gotAuth atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer ts.Close()
	c := NewClient(ts.URL, "", "m") // 无 key
	c.http = ts.Client()
	if _, err := c.Chat(t.Context(), ChatRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if gotAuth.Load().(string) != "" {
		t.Fatalf("Authorization 不应发送，got %q", gotAuth.Load().(string))
	}
}

func TestChatStreamNonJSONLinesSkipped(t *testing.T) {
	ts, _ := mockServer(t, func(w http.ResponseWriter, _ *http.Request, _ *reqCapture) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: not json\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	defer ts.Close()
	c := newClient(ts)
	c.SetStream(true)
	comp, err := c.Chat(t.Context(), ChatRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Content != "ok" {
		t.Fatalf("content = %q", comp.Content)
	}
}
