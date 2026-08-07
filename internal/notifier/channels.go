package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// redactQuery 抹除错误信息中 URL 的查询串：钉钉/飞书/企微的 access_token/key 常在 query 中，
// 避免密钥随审计 metadata / 日志持久化。
var redactQuery = regexp.MustCompile(`\?[^\s"']*`)

func redact(s string) string { return redactQuery.ReplaceAllString(s, "?***") }

// postJSON POST JSON 到 webhook；非 2xx 视为失败（返回状态码与响应摘要，URL 已脱敏）。
func postJSON(ctx context.Context, client *http.Client, url string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook 请求失败: %s", redact(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook 返回 %s: %s", resp.Status, strings.TrimSpace(string(rb)))
	}
	return nil
}

// renderBody 生成 markdown 正文（钉钉/企微/飞书通用），追加事故 ID/追踪 ID/详情链接。
func renderBody(msg Message) string {
	var b strings.Builder
	b.WriteString(msg.Content)
	if msg.IncidentID != "" {
		b.WriteString("\n\n**关联事故**：`" + msg.IncidentID + "`")
	}
	if msg.CorrelationID != "" {
		b.WriteString("\n**追踪ID**：`" + msg.CorrelationID + "`")
	}
	if msg.URL != "" {
		b.WriteString("\n**详情**：[查看](" + msg.URL + ")")
	}
	return b.String()
}

// DingTalk 钉钉群机器人（自定义机器人 webhook，markdown 消息）。
type DingTalk struct {
	webhook string
	client  *http.Client
}

// NewDingTalk 创建钉钉渠道。webhook 为钉钉机器人完整地址。
func NewDingTalk(webhook string, client *http.Client) *DingTalk {
	return &DingTalk{webhook: webhook, client: client}
}

// Name 渠道名。
func (d *DingTalk) Name() string { return "dingtalk" }

// Send 发送 markdown 消息。
func (d *DingTalk) Send(ctx context.Context, msg Message) error {
	return postJSON(ctx, d.client, d.webhook, map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": msg.Title,
			"text":  renderBody(msg),
		},
	})
}

// Feishu 飞书群机器人（自定义机器人 webhook，interactive 卡片）。
type Feishu struct {
	webhook string
	client  *http.Client
}

// NewFeishu 创建飞书渠道。webhook 为飞书机器人完整地址。
func NewFeishu(webhook string, client *http.Client) *Feishu {
	return &Feishu{webhook: webhook, client: client}
}

// Name 渠道名。
func (f *Feishu) Name() string { return "feishu" }

// Send 发送交互卡片，主题色按严重度映射。
func (f *Feishu) Send(ctx context.Context, msg Message) error {
	return postJSON(ctx, f.client, f.webhook, map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"header": map[string]any{
				"title":    map[string]string{"tag": "plain_text", "content": msg.Title},
				"template": feishuColor(msg.Severity),
			},
			"elements": []any{
				map[string]any{"tag": "markdown", "content": renderBody(msg)},
			},
		},
	})
}

// feishuColor 严重度 → 卡片主题色。
func feishuColor(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	}
	return "blue"
}

// Wecom 企业微信群机器人（自定义机器人 webhook，markdown 消息）。
type Wecom struct {
	webhook string
	client  *http.Client
}

// NewWecom 创建企微渠道。webhook 为企微机器人完整地址。
func NewWecom(webhook string, client *http.Client) *Wecom {
	return &Wecom{webhook: webhook, client: client}
}

// Name 渠道名。
func (w *Wecom) Name() string { return "wecom" }

// Send 发送 markdown 消息（企微 markdown 需自带标题）。
func (w *Wecom) Send(ctx context.Context, msg Message) error {
	return postJSON(ctx, w.client, w.webhook, map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]string{"content": "**【" + msg.Title + "】**\n" + renderBody(msg)},
	})
}

// HTTP 通用 webhook：原样 POST Message（JSON），可接入任意自定义接收端/本地 mock。
type HTTP struct {
	webhook string
	client  *http.Client
}

// NewHTTP 创建通用渠道。webhook 为自定义接收地址。
func NewHTTP(webhook string, client *http.Client) *HTTP {
	return &HTTP{webhook: webhook, client: client}
}

// Name 渠道名。
func (h *HTTP) Name() string { return "http" }

// Send 原样 POST Message。
func (h *HTTP) Send(ctx context.Context, msg Message) error {
	return postJSON(ctx, h.client, h.webhook, msg)
}
