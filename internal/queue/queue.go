// Package queue 定义异步任务队列抽象。MVP 用内存 worker 池，扩展可换 asynq+Redis。
package queue

import (
	"context"
	"encoding/json"
)

// 任务类型。
const (
	TaskWorkflowTrigger = "workflow.trigger"
	TaskAgentRun        = "agent.run"
	TaskNotifySend      = "notify.send" // 告警级通知（KEEPON 5.4 节流后发送）
)

// Task 队列任务。
type Task struct {
	Type     string          `json:"type"`
	TenantID string          `json:"tenantId"`
	Payload  json.RawMessage `json:"payload"`
}

// Handler 任务处理函数。
type Handler func(ctx context.Context, t Task) error

// Queue 异步任务队列接口。
type Queue interface {
	Enqueue(ctx context.Context, t Task) error
	// Consume 常驻消费（阻塞直到 ctx 取消）。
	Consume(ctx context.Context, h Handler) error
	Close() error
}
