// Package notifier 通知渠道：审批/告警/处置结果经钉钉/飞书/企微/通用 webhook 推送。
// 通知与审计联动。设计要点：
//   - 通知不阻断主流程（fail-open）：任一渠道失败只记审计，不影响审批闭环；
//   - 未配置 webhook 的渠道自动跳过（Len()==0 时 Notify 返回空结果）；
//   - 各渠道负责把 Message 渲染为对应格式（钉钉/企微 markdown、飞书卡片、通用 JSON）。
package notifier

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Message 统一通知内容（渠道无关，各 Channel 自行渲染）。
type Message struct {
	Title         string            `json:"title"`    // 标题（事故名/告警名）
	Content       string            `json:"content"`  // markdown 正文
	Severity      string            `json:"severity"` // critical|high|medium|low|info
	IncidentID    string            `json:"incidentId,omitempty"`
	CorrelationID string            `json:"correlationId,omitempty"`
	URL           string            `json:"url,omitempty"` // 前端详情链接（未给时按 baseURL 拼接）
	Actor         string            `json:"actor,omitempty"`
	Action        string            `json:"action,omitempty"` // pending_approval|approved|rejected|created|resolved|test
	Extra         map[string]string `json:"extra,omitempty"`
}

// Channel 通知渠道。Send 为一次尝试，失败返回 error（Service 聚合结果，不中断）。
type Channel interface {
	Name() string
	Send(ctx context.Context, msg Message) error
}

// Result 单渠道通知结果（审计 metadata 用）。
type Result struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// Service 通知路由：向全部已注册渠道发送，逐渠道返回结果。
type Service struct {
	mu       sync.RWMutex
	channels map[string]Channel
	baseURL  string       // 前端地址，用于拼接 incident 详情链接
	client   *http.Client // 渠道共享 HTTP 客户端（Reload 重建渠道时复用）
}

// NewService 创建通知路由。baseURL 为空则不生成详情链接。
func NewService(baseURL string) *Service {
	return &Service{
		channels: map[string]Channel{},
		baseURL:  baseURL,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// New 按配置创建通知服务：channels 为逗号分隔的渠道名（dingtalk/feishu/wecom/http），
// 对应 webhook 为空的渠道自动跳过（未配置时 Notify 返回空结果，仅审计提示）。
func New(channels, dingtalk, feishu, wecom, httpWebhook, baseURL string) *Service {
	svc := NewService(baseURL)
	client := &http.Client{Timeout: 10 * time.Second}
	for _, name := range strings.Split(channels, ",") {
		switch strings.TrimSpace(name) {
		case "dingtalk":
			if dingtalk != "" {
				svc.Add(NewDingTalk(dingtalk, client))
			}
		case "feishu":
			if feishu != "" {
				svc.Add(NewFeishu(feishu, client))
			}
		case "wecom":
			if wecom != "" {
				svc.Add(NewWecom(wecom, client))
			}
		case "http":
			if httpWebhook != "" {
				svc.Add(NewHTTP(httpWebhook, client))
			}
		}
	}
	return svc
}

// Add 注册渠道（同名重复注册以后者为准）。
func (s *Service) Add(ch Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[ch.Name()] = ch
}

// Len 已注册渠道数（未配置为 0）。
func (s *Service) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.channels)
}

// Enabled 已注册渠道名（稳定排序，便于日志/审计展示）。
func (s *Service) Enabled() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.channels))
	for n := range s.channels {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Reload 用渠道配置重建已注册渠道（热更新，不换指针，编排器/API 引用保持有效）。
// 未启用或 webhook 为空的配置跳过；配置来源为 DB（notifier.ConfigService.Reload 调用）。
func (s *Service) Reload(configs []ChannelConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(map[string]Channel, len(configs))
	for i := range configs {
		c := &configs[i]
		if !c.Enabled || c.Webhook == "" {
			continue
		}
		switch c.Name {
		case "dingtalk":
			ch[c.Name] = NewDingTalk(c.Webhook, s.client)
		case "feishu":
			ch[c.Name] = NewFeishu(c.Webhook, s.client)
		case "wecom":
			ch[c.Name] = NewWecom(c.Webhook, s.client)
		case "http":
			ch[c.Name] = NewHTTP(c.Webhook, s.client)
		}
	}
	s.channels = ch
}

// Notify 向全部渠道发送；结果顺序与 Enabled() 一致，任一失败不影响其他渠道。
// 快照渠道引用后释放锁再发送（避免嵌套 RLock 与 Reload 的写锁互相等待造成死锁，也不持锁做网络 IO）。
func (s *Service) Notify(ctx context.Context, msg Message) []Result {
	if msg.URL == "" && msg.IncidentID != "" && s.baseURL != "" {
		msg.URL = s.baseURL + "/incidents/" + msg.IncidentID
	}
	s.mu.RLock()
	names := make([]string, 0, len(s.channels))
	for n := range s.channels {
		names = append(names, n)
	}
	sort.Strings(names)
	snapshot := make([]Channel, len(names))
	for i, n := range names {
		snapshot[i] = s.channels[n]
	}
	s.mu.RUnlock()

	results := make([]Result, 0, len(snapshot))
	for _, ch := range snapshot {
		r := Result{Channel: ch.Name()}
		if err := ch.Send(ctx, msg); err != nil {
			r.Error = err.Error()
		} else {
			r.OK = true
		}
		results = append(results, r)
	}
	return results
}

// AllOK 全部渠道发送成功（空结果视为成功，是否通知过由调用方按 Len() 判断）。
func (s *Service) AllOK(results []Result) bool {
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
}
