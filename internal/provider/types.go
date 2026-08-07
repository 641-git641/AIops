package provider

import (
	"context"
	"net/http"

	"aiops/internal/alert"
)

// Provider 数据源抽象。所有内置 provider 实现该接口，按能力再实现
// PullProvider / ConsumerProvider / WebhookProvider 之一或多个（运行时类型断言）。
type Provider interface {
	Kind() string // "prometheus" | "aliyun-cloudmon" | "dingtalk" | "llm" ...
	Name() string
}

// PullProvider 定时拉取型（cron 调度）：返回归一化入站事件。
type PullProvider interface {
	Provider
	Pull(ctx context.Context) ([]*alert.IngestEvent, error)
}

// ConsumerProvider 长连接型：启动后常驻，通过 sink 通道推入管线。
type ConsumerProvider interface {
	Provider
	Start(ctx context.Context, sink chan<- *alert.IngestEvent) error
}

// WebhookProvider push 型：解析 HTTP 请求体 → 归一化入站事件。
type WebhookProvider interface {
	Provider
	Parse(body []byte, r *http.Request) ([]*alert.IngestEvent, error)
}
