// Package circuit 实现熔断器（安全护栏链之一）。
// 三态 CLOSED → (连续失败达阈值) → OPEN(冷却期) → HALF_OPEN(试探) → CLOSED/OPEN。
// 自愈执行前必须通过 Allow()，熔断期间动作被阻断（fail-safe），防止故障级联。
package circuit

import (
	"sync"
	"time"
)

// State 熔断器状态。
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	}
	return "unknown"
}

// Breaker 熔断器：线程安全。threshold 次连续失败后开启，cooldown 后进入半开试探。
type Breaker struct {
	mu            sync.Mutex
	state         State
	failures      int
	threshold     int
	cooldown      time.Duration
	openUntil     time.Time
	probeInFlight bool // 半开试探是否在途（只放行一个）
	now           func() time.Time
}

// NewBreaker 创建熔断器。
func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}
	return &Breaker{state: StateClosed, threshold: threshold, cooldown: cooldown, now: time.Now}
}

// Allow 判定本次调用是否放行。CLOSED 放行；OPEN 到冷却期则转入 HALF_OPEN 并放行一次试探。
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if !b.now().Before(b.openUntil) { // 冷却期已过 → 半开试探
			b.state = StateHalfOpen
			b.probeInFlight = true
			return true
		}
		return false
	case StateHalfOpen:
		if b.probeInFlight {
			return false // 试探在途，其余调用继续熔断
		}
		b.probeInFlight = true
		return true
	}
	return true
}

// Success 记录一次成功（半开试探成功 → CLOSED，计数清零）。
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.probeInFlight = false
	b.state = StateClosed
}

// Failure 记录一次失败。半开试探失败或连续失败达阈值 → OPEN。
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.state == StateHalfOpen || b.failures >= b.threshold {
		b.state = StateOpen
		b.openUntil = b.now().Add(b.cooldown)
		b.probeInFlight = false
	}
}

// State 返回当前状态。
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Failures 返回连续失败次数。
func (b *Breaker) Failures() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures
}
