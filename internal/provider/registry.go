package provider

import (
	"fmt"
	"sort"
	"sync"
)

// Registry provider 注册表。内置 provider 在各自包 init() 中 Register，
// main 聚合 import 触发注册（Go 惯用法替代 keep 的 importlib 动态导入）。
type Registry struct {
	mu sync.RWMutex
	m  map[string]Provider
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{m: map[string]Provider{}}
}

// Register 注册 provider。
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[p.Kind()] = p
}

// Get 按 kind 获取 provider。
func (r *Registry) Get(kind string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.m[kind]
	return p, ok
}

// MustWebhook 获取并断言为 WebhookProvider。
func (r *Registry) MustWebhook(kind string) (WebhookProvider, error) {
	p, ok := r.Get(kind)
	if !ok {
		return nil, fmt.Errorf("provider %q not registered", kind)
	}
	w, ok := p.(WebhookProvider)
	if !ok {
		return nil, fmt.Errorf("provider %q is not a webhook provider", kind)
	}
	return w, nil
}

// Kinds 返回已注册的 provider 列表（排序，便于展示）。
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ks := make([]string, 0, len(r.m))
	for k := range r.m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
