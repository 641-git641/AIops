package queue

import (
	"context"
	"sync"
	"sync/atomic"
)

// MemoryQueue 进程内 worker 池队列（单机零依赖）。
type MemoryQueue struct {
	ch      chan Task
	workers int
	wg      sync.WaitGroup
	closed  atomic.Bool
}

// NewMemoryQueue 创建带 N 个 worker 的内存队列。
func NewMemoryQueue(workers int) *MemoryQueue {
	if workers <= 0 {
		workers = 4
	}
	return &MemoryQueue{ch: make(chan Task, 256), workers: workers}
}

// Enqueue 投递任务（非阻塞，队列满则丢弃并返回错误由上层兜底）。
func (q *MemoryQueue) Enqueue(_ context.Context, t Task) error {
	select {
	case q.ch <- t:
		return nil
	default:
		return errQueueFull
	}
}

// Consume 启动 worker 消费，阻塞直到 ctx 取消。
func (q *MemoryQueue) Consume(ctx context.Context, h Handler) error {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.worker(ctx, h)
	}
	<-ctx.Done()
	q.wg.Wait()
	return nil
}

func (q *MemoryQueue) worker(ctx context.Context, h Handler) {
	defer q.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case t, ok := <-q.ch:
			if !ok {
				return
			}
			// MVP：失败仅记录，不重试（工作流/Agent 侧有自己的幂等与重试）
			_ = h(ctx, t)
		}
	}
}

// Len 当前队列深度（指标采样用；竞态下近似值即可）。
func (q *MemoryQueue) Len() int { return len(q.ch) }

// Close 关闭队列。
func (q *MemoryQueue) Close() error {
	if q.closed.CompareAndSwap(false, true) {
		close(q.ch)
	}
	return nil
}

var errQueueFull = queueFullError{}

type queueFullError struct{}

func (queueFullError) Error() string { return "queue: queue full" }
