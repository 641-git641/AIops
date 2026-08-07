package eventbus

import (
	"context"
	"sync"
)

// MemoryBus 内存事件总线：发布同步调用订阅者（失败不中断）。
type MemoryBus struct {
	mu          sync.RWMutex
	subscribers map[string][]Handler
}

// NewMemoryBus 创建内存总线。
func NewMemoryBus() *MemoryBus {
	return &MemoryBus{subscribers: map[string][]Handler{}}
}

// Publish 同步发布事件到所有订阅者。
func (b *MemoryBus) Publish(_ context.Context, e *Event) error {
	b.mu.RLock()
	hs := b.subscribers[e.Topic]
	b.mu.RUnlock()
	for _, h := range hs {
		_ = h(context.Background(), e) // MVP：订阅失败仅忽略；审计类订阅应容错
	}
	return nil
}

// Subscribe 注册订阅者。
func (b *MemoryBus) Subscribe(_ context.Context, topic, _ string, h Handler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[topic] = append(b.subscribers[topic], h)
	return nil
}

// Close 空操作。
func (b *MemoryBus) Close() error { return nil }
