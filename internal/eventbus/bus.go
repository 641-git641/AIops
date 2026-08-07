package eventbus

import (
	"context"
)

// Event 总线上的事件。
type Event struct {
	ID            string         `json:"id"`
	Topic         string         `json:"topic"`
	Type          string         `json:"type"` // alert.fired / rca.completed / heal.proposed / change.approved ...
	CorrelationID string         `json:"correlationId"`
	TenantID      string         `json:"tenantId"`
	IncidentID    string         `json:"incidentId,omitempty"`
	Payload       map[string]any `json:"payload"`
	Timestamp     int64          `json:"timestamp"` // millis
}

// Handler 订阅处理函数。
type Handler func(ctx context.Context, e *Event) error

// Bus 事件总线抽象。MVP 用内存实现；生产可换 Kafka（acks=all + 手动 commit + DLQ）。
type Bus interface {
	Publish(ctx context.Context, e *Event) error
	Subscribe(ctx context.Context, topic, groupID string, h Handler) error
	Close() error
}
