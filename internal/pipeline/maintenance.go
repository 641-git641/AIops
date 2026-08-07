package pipeline

import (
	"context"
	"sync"
	"time"

	"aiops/internal/alert"
	"aiops/internal/maintenance"
)

// MaintenanceFilter 维护窗口过滤接口。MVP 默认放行；接入 WindowFilter 后按活跃窗口静默。
type MaintenanceFilter interface {
	// IsSuppressed 判定告警是否处于维护窗口静默期。
	IsSuppressed(ctx context.Context, a *alert.Alert) (bool, error)
}

// NoopMaintenance 维护窗口空实现（总是放行，未装配过滤器时兜底）。
type NoopMaintenance struct{}

// IsSuppressed 恒 false（不静默）。
func (NoopMaintenance) IsSuppressed(_ context.Context, _ *alert.Alert) (bool, error) {
	return false, nil
}

// WindowFilter 基于维护窗口表的过滤器。热路径按租户缓存活跃窗口（TTL 内不重复查库），
// 窗口 CRUD 后调用 Invalidate 立即失效对应租户缓存。
type WindowFilter struct {
	svc *maintenance.Service
	ttl time.Duration

	mu    sync.RWMutex
	cache map[string]windowCache // tenantID → 缓存
}

type windowCache struct {
	windows []maintenance.Window
	fetched time.Time
}

// NewWindowFilter 创建维护窗口过滤器。ttl 为活跃窗口缓存有效期（0 则每次查库）。
func NewWindowFilter(svc *maintenance.Service, ttl time.Duration) *WindowFilter {
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	return &WindowFilter{svc: svc, ttl: ttl, cache: make(map[string]windowCache)}
}

// IsSuppressed 返回告警是否被任一活跃窗口命中。
func (f *WindowFilter) IsSuppressed(ctx context.Context, a *alert.Alert) (bool, error) {
	ws, err := f.activeWindows(ctx, a.TenantID)
	if err != nil {
		return false, err
	}
	for i := range ws {
		if f.svc.Matches(&ws[i], a) {
			return true, nil
		}
	}
	return false, nil
}

// Invalidate 使某租户的活跃窗口缓存失效（窗口 CRUD 后调用）。
func (f *WindowFilter) Invalidate(tenantID string) {
	f.mu.Lock()
	delete(f.cache, tenantID)
	f.mu.Unlock()
}

func (f *WindowFilter) activeWindows(ctx context.Context, tenantID string) ([]maintenance.Window, error) {
	// 快路径：读锁只做缓存命中判断（读多写少，毫秒级返回，不碰 DB）。
	// 压测观测：若在锁内执行 DB 查询，缓存过期瞬间所有并发请求会全局排队
	// （持有者还要等连接池），导致周期性吞吐崩塌——故查库必须在锁外完成。
	f.mu.RLock()
	if e, ok := f.cache[tenantID]; ok && time.Since(e.fetched) < f.ttl {
		f.mu.RUnlock()
		return e.windows, nil
	}
	f.mu.RUnlock()

	// 慢路径：锁外查库（并发请求可能同时触发，但每个租户至多一次；查库无锁竞争）
	ws, err := f.svc.ActiveWindows(ctx, tenantID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.cache[tenantID]; ok && time.Since(e.fetched) < f.ttl {
		return e.windows, nil // 已被其他请求回填更新鲜，直接返回
	}
	f.cache[tenantID] = windowCache{windows: ws, fetched: time.Now()}
	return ws, nil
}
