package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// RCAInput RCA LLM 增强的输入：告警 + 确定性证据链（供 RCAAgent 组装）。
type RCAInput struct {
	Service    string            `json:"service"`
	AlertName  string            `json:"alertName"`
	Severity   string            `json:"severity"`
	Summary    string            `json:"summary"`
	Labels     map[string]string `json:"labels"`
	RootCause  string            `json:"rootCause"` // 确定性根因候选
	Confidence float64           `json:"confidence"`
	Evidence   []string          `json:"evidence"`
	Changes    []string          `json:"changes"`
	Paths      []string          `json:"paths"`
}

// RCAOutput LLM 补充的自然语言根因分析结论（写入 rca 节点结果的 llm* 字段）。
type RCAOutput struct {
	RootCause        string   `json:"rootCause"`
	Rationale        string   `json:"rationale"`
	SuggestedActions []string `json:"suggestedActions"`
}

// ClusterInput AI 聚类摘要增强的输入：告警簇事实（供 ai.Service 可选装配）。
type ClusterInput struct {
	Service   string   `json:"service"`
	Severity  string   `json:"severity"`
	Names     []string `json:"names"`
	Count     int      `json:"count"`
	StartedAt string   `json:"startedAt"` // RFC3339
	LastSeen  string   `json:"lastSeen"`  // RFC3339
}

// ClusterOutput LLM 补充的聚类自然语言摘要（非确定性模板，写入 Incident.LLMSummary）。
type ClusterOutput struct {
	Summary string `json:"summary"`
}

// ClusterLLM AI 聚类摘要增强器接口（由 *Client 实现；未配置时 Service.clusterLLM 为 nil，走确定性 Summarize）。
type ClusterLLM interface {
	EnhanceCluster(ctx context.Context, in ClusterInput) (*ClusterOutput, error)
}

// Client OpenAI 兼容 Chat Completions 客户端（DeepSeek / Ollama / vLLM / LiteLLM 等）。
// 未配置 apiKey 或调用失败时由调用方降级为确定性结论（fail-open，不阻塞编排）。
// stream=true 时走 SSE 流式（逐 delta 累计，避免大模型长输出被整体超时打断）。
type Client struct {
	baseURL string
	apiKey  string
	model   string
	stream  bool
	http    *http.Client // Timeout 覆盖整体调用超时（含流式累计期间）
}

// NewClient 创建 LLM 客户端。baseURL 默认 https://api.deepseek.com，model 默认 deepseek-chat。
// 超时默认 15s，可通过 SetTimeout 覆盖（超时可控）；默认非流式，SetStream(true) 开启 SSE。
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

// SetStream 开关 SSE 流式响应（openai-compat 端点普遍支持；不支持流式的端点保持关闭）。
func (c *Client) SetStream(s bool) { c.stream = s }

// chatURL 拼出 /chat/completions 端点（baseURL 已含该路径则不重复追加）。
func (c *Client) chatURL() string {
	if strings.HasSuffix(c.baseURL, "/chat/completions") {
		return c.baseURL
	}
	return c.baseURL + "/chat/completions"
}

// chatRequest OpenAI 兼容请求体（temperature 压低，RCA 要确定性优先）。
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// chat 按客户端配置选择流式/非流式完成。
func (c *Client) chat(ctx context.Context, system, user string) (string, error) {
	if c.stream {
		return c.CompleteStream(ctx, system, user)
	}
	return c.Complete(ctx, system, user)
}

// Complete 单轮对话返回模型文本（system + user，非流式）。
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	body, _ := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature: 0.3,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", llmStatusError(resp)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out chatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode llm response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm empty choices")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// CompleteStream 单轮对话返回模型文本（SSE 流式）：逐行解析 `data:` 事件，
// 累计 choices[].delta.content，遇 `data: [DONE]` 结束。整体受 SetTimeout 约束。
func (c *Client) CompleteStream(ctx context.Context, system, user string) (string, error) {
	body, _ := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature: 0.3,
		Stream:      true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", llmStatusError(resp)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024) // 行可能较长（大 JSON chunk）
	var sb strings.Builder
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
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 非 JSON 的事件行跳过（健壮性）
		}
		for _, ch := range chunk.Choices {
			sb.WriteString(ch.Delta.Content)
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return strings.TrimSpace(sb.String()), nil
}

// llmStatusError 先查状态码再解析错误体（错误体非 JSON 时仍返回状态码信息）。
func llmStatusError(resp *http.Response) error {
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var errResp struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != nil {
		msg = errResp.Error.Message
	}
	return fmt.Errorf("llm status %d: %s", resp.StatusCode, msg)
}

const rcaSystemPrompt = "你是资深 SRE 根因分析专家。给定一条告警与已收集的确定性证据，用简体中文输出 JSON：" +
	`{"rootCause":"最可能根因","rationale":"简短分析依据","suggestedActions":["建议动作1","建议动作2"]}` +
	"。只输出 JSON，不要任何多余文字或 markdown 代码块标记。"

// rcaUserPrompt 把告警 + 确定性证据组装成用户输入。
func rcaUserPrompt(in RCAInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "告警：%s（severity=%s, service=%s）", in.AlertName, in.Severity, in.Service)
	if in.Summary != "" {
		fmt.Fprintf(&b, "\n摘要：%s", in.Summary)
	}
	if len(in.Labels) > 0 {
		fmt.Fprintf(&b, "\n标签：%s", formatLabels(in.Labels))
	}
	if in.RootCause != "" {
		fmt.Fprintf(&b, "\n确定性根因候选：%s（置信度 %.2f）", in.RootCause, in.Confidence)
	}
	if len(in.Evidence) > 0 {
		fmt.Fprintf(&b, "\n证据：\n- %s", strings.Join(in.Evidence, "\n- "))
	}
	if len(in.Changes) > 0 {
		fmt.Fprintf(&b, "\n近期变更：\n- %s", strings.Join(in.Changes, "\n- "))
	}
	if len(in.Paths) > 0 {
		fmt.Fprintf(&b, "\n依赖路径：\n- %s", strings.Join(in.Paths, "\n- "))
	}
	b.WriteString("\n\n请结合以上证据给出最可能根因与建议动作。")
	return b.String()
}

// EnhanceRCA 让 LLM 基于确定性证据补充自然语言根因与建议动作。
// 失败返回 error（调用方 fail-open 降级为确定性结论）。
func (c *Client) EnhanceRCA(ctx context.Context, in RCAInput) (*RCAOutput, error) {
	content, err := c.chat(ctx, rcaSystemPrompt, rcaUserPrompt(in))
	if err != nil {
		return nil, err
	}
	return parseRCAOutput(content)
}

// parseRCAOutput 解析模型输出：容忍 ```json 围栏与前后空白，rootCause 缺失视为解析失败。
func parseRCAOutput(content string) (*RCAOutput, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	var out RCAOutput
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, fmt.Errorf("parse llm rca output: %w", err)
	}
	if out.RootCause == "" {
		return nil, fmt.Errorf("llm rca output missing rootCause")
	}
	return &out, nil
}

const clusterSystemPrompt = "你是资深 SRE 告警分析师。给定一组已聚类的关联告警，用简体中文输出 JSON：" +
	`{"summary":"50-80 字的自然语言摘要，概括这组告警反映的故障类型、可能原因与建议关注点"}` +
	"。只输出 JSON，不要任何多余文字或 markdown 代码块标记。"

// clusterUserPrompt 把告警簇事实组装成用户输入。
func clusterUserPrompt(in ClusterInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "告警簇：服务 %s，最高严重度 %s，%d 条关联告警", in.Service, in.Severity, in.Count)
	if len(in.Names) > 0 {
		fmt.Fprintf(&b, "\n告警：%s", strings.Join(in.Names, "、"))
	}
	if in.StartedAt != "" && in.LastSeen != "" {
		fmt.Fprintf(&b, "\n时间范围：%s ~ %s", in.StartedAt, in.LastSeen)
	}
	b.WriteString("\n\n请概括这一组告警。")
	return b.String()
}

// EnhanceCluster 让 LLM 生成聚类事故的自然语言摘要（非确定性模板；失败由调用方 fallback 确定性摘要）。
func (c *Client) EnhanceCluster(ctx context.Context, in ClusterInput) (*ClusterOutput, error) {
	content, err := c.chat(ctx, clusterSystemPrompt, clusterUserPrompt(in))
	if err != nil {
		return nil, err
	}
	return parseClusterOutput(content)
}

// parseClusterOutput 解析模型输出：容忍 ```json 围栏与前后空白，summary 缺失视为解析失败。
func parseClusterOutput(content string) (*ClusterOutput, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	var out ClusterOutput
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, fmt.Errorf("parse llm cluster output: %w", err)
	}
	if out.Summary == "" {
		return nil, fmt.Errorf("llm cluster output missing summary")
	}
	return &out, nil
}

// formatLabels 标签按 key 排序拼为 k=v 列表（prompt 稳定，便于缓存/对比）。
func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ", ")
}
