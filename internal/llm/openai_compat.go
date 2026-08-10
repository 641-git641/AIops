package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client OpenAI 兼容 Chat Completions 客户端（风格对齐 internal/ai/llm.go）：
// 支持通用 messages 数组、tools/tool_calls、流式增量回调。
// 未配置 apiKey 时不发 Authorization 头（本地 mock 场景）。
type Client struct {
	baseURL string
	apiKey  string
	model   string
	stream  bool
	http    *http.Client // Timeout 覆盖整体调用超时（含流式累计期间）
}

// NewClient 创建客户端。baseURL/model 缺省对齐 ai.Client（deepseek 默认）；
// 超时默认 15s（SetTimeout 覆盖）；默认非流式（SetStream(true) 开启 SSE）。
func NewClient(baseURL, apiKey, model string) *Client {
	if baseURL == "" {
		baseURL = "https://api.deepseek.com"
	}
	if model == "" {
		model = "deepseek-chat"
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// SetTimeout 覆盖整体调用超时（含流式累计期间）。d<=0 时忽略，保持现值。
func (c *Client) SetTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	c.http.Timeout = d
}

// SetStream 开关 SSE 流式响应。
func (c *Client) SetStream(s bool) { c.stream = s }

// chatURL 拼出 /chat/completions 端点（baseURL 已含该路径则不重复追加）。
func (c *Client) chatURL() string {
	if strings.HasSuffix(c.baseURL, "/chat/completions") {
		return c.baseURL
	}
	return c.baseURL + "/chat/completions"
}

// ChatRequest OpenAI 兼容请求体。
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature float64   `json:"temperature"`
	Stream      bool      `json:"stream"`
}

// chatResponse 非流式响应。
type chatResponse struct {
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string     `json:"role"`
			Content   *string    `json:"content"` // tool_calls 时可能为 null
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// streamChunk SSE 单 chunk（流式）。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string          `json:"content"`
			ToolCalls []DeltaToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// Chat 多轮对话（可选工具）。流式（SetStream(true)）时逐 chunk 回调 onDelta
// （nil 可）；返回拼接后的 Completion：content 累计、tool_calls 按 index 合并、
// finish_reason/usage 提取。错误语义对齐 ai.Client（状态码 + 错误体）。
func (c *Client) Chat(ctx context.Context, req ChatRequest, onDelta func(Delta)) (*Completion, error) {
	req.Model = c.model
	req.Stream = c.stream
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, llmStatusError(resp)
	}
	if c.stream {
		return c.readStream(resp.Body, onDelta)
	}
	return c.readComplete(resp.Body)
}

// readComplete 非流式响应解析。
func (c *Client) readComplete(body io.Reader) (*Completion, error) {
	raw, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("llm: read: %w", err)
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("llm: parse: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("llm: empty choices")
	}
	ch := out.Choices[0]
	comp := &Completion{FinishReason: FinishReason(ch.FinishReason), Usage: out.Usage}
	if ch.Message.Content != nil {
		comp.Content = strings.TrimSpace(*ch.Message.Content)
	}
	comp.ToolCalls = ch.Message.ToolCalls
	return comp, nil
}

// acc 流式拼接累加器（tool_calls 按 index 合并跨 chunk 碎片）。
type acc struct {
	content   strings.Builder
	toolCalls []*accTool // 按 index 对齐
	finish    FinishReason
	usage     *Usage
}

type accTool struct {
	id, name string
	args     strings.Builder
}

// readStream SSE 流式解析：逐 `data:` 行，[DONE] 结束，非 JSON 行跳过。
func (c *Client) readStream(body io.Reader, onDelta func(Delta)) (*Completion, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024) // 行可能较长（大 JSON chunk）
	var a acc
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		if data == "" {
			continue
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 非 JSON 事件行跳过（健壮性）
		}
		var d Delta
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				a.content.WriteString(ch.Delta.Content)
				d.Content += ch.Delta.Content
			}
			for _, tc := range ch.Delta.ToolCalls {
				d.ToolCalls = append(d.ToolCalls, tc)
				applyDeltaToolCall(&a, tc)
			}
			if ch.FinishReason != "" {
				a.finish = FinishReason(ch.FinishReason)
			}
		}
		if chunk.Usage != nil {
			a.usage = chunk.Usage
		}
		if onDelta != nil && (d.Content != "" || len(d.ToolCalls) > 0) {
			onDelta(d)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("llm: stream: %w", err)
	}
	comp := &Completion{Content: strings.TrimSpace(a.content.String()), FinishReason: a.finish, Usage: a.usage}
	for _, t := range a.toolCalls {
		comp.ToolCalls = append(comp.ToolCalls, ToolCall{
			ID: t.id, Type: "function",
			Function: ToolCallFunc{Name: t.name, Arguments: t.args.String()},
		})
	}
	return comp, nil
}

// applyDeltaToolCall 把工具调用增量合并进累加器（index 对齐）。
func applyDeltaToolCall(a *acc, tc DeltaToolCall) {
	for len(a.toolCalls) <= tc.Index {
		a.toolCalls = append(a.toolCalls, &accTool{})
	}
	t := a.toolCalls[tc.Index]
	if tc.ID != "" {
		t.id = tc.ID
	}
	if tc.Function.Name != "" {
		t.name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		t.args.WriteString(tc.Function.Arguments)
	}
}
