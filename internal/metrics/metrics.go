// Package metrics 提供轻量 Prometheus 文本格式指标出口（Phase 3 规模化）。
// 零第三方依赖实现：counter（带固定标签集）+ histogram（累计桶）+ gauge（采集时求值），
// 供压测观测 /metrics（吞吐、去重构成、管线耗时分位、队列深度）。
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Registry 指标注册表（线程安全）。指标名需全局唯一。
type Registry struct {
	mu         sync.Mutex
	counters   map[string]*counter
	histograms map[string]*histogram
	gauges     map[string]*gauge
}

// counter 计数指标：labels 为固定标签集，values 按标签值组合展开。
type counter struct {
	name   string
	help   string
	labels []string
	values map[string]float64
}

// histogram 直方图：累计桶（Prometheus le 语义）。
type histogram struct {
	name    string
	help    string
	buckets []float64 // 上界递增
	counts  []uint64  // 桶累计计数（与 buckets 一一对应）
	sum     float64
	count   uint64
}

// gauge 采集时求值（如队列深度）。
type gauge struct {
	name string
	help string
	fn   func() float64
}

// SecondsBuckets 管线耗时直方图桶（秒）。
var SecondsBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// New 创建注册表。
func New() *Registry {
	return &Registry{
		counters:   map[string]*counter{},
		histograms: map[string]*histogram{},
		gauges:     map[string]*gauge{},
	}
}

// Counter 注册计数指标（固定标签集）。
func (r *Registry) Counter(name, help string, labels ...string) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name] = &counter{name: name, help: help, labels: labels, values: map[string]float64{}}
	return r
}

// Inc 计数 +1（labelValues 需与注册时的标签一一对应）。
func (r *Registry) Inc(name string, labelValues ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[name]
	if !ok {
		return
	}
	c.values[strings.Join(labelValues, "\x00")]++
}

// Histogram 注册直方图。
func (r *Registry) Histogram(name, help string, buckets []float64) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.histograms[name] = &histogram{name: name, help: help, buckets: append([]float64(nil), buckets...), counts: make([]uint64, len(buckets))}
	return r
}

// Observe 记录一个观测值。
func (r *Registry) Observe(name string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.histograms[name]
	if !ok {
		return
	}
	h.sum += v
	h.count++
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
}

// Gauge 注册采集时求值的仪表指标（如队列深度）。
func (r *Registry) Gauge(name, help string, fn func() float64) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[name] = &gauge{name: name, help: help, fn: fn}
	return r
}

// WriteTo 输出 Prometheus 文本格式（指标按名排序，输出稳定可 diff）。
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range sortedKeys(r.counters) {
		c := r.counters[name]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
		for _, k := range sortedKeys(c.values) {
			fmt.Fprintf(w, "%s%s %g\n", c.name, labelSuffix(c.labels, k), c.values[k])
		}
	}
	for _, name := range sortedKeys(r.histograms) {
		h := r.histograms[name]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
		for i, b := range h.buckets {
			fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", h.name, trimZero(b), h.counts[i])
		}
		// Prometheus 规范：+Inf 桶必须等于 count（覆盖超出最大桶的样本）
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", h.name, h.count)
		fmt.Fprintf(w, "%s_sum %g\n%s_count %d\n", h.name, h.sum, h.name, h.count)
	}
	for _, name := range sortedKeys(r.gauges) {
		g := r.gauges[name]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		fmt.Fprintf(w, "%s %g\n", g.name, g.fn())
	}
	return nil
}

// labelSuffix 由标签值与注册标签集渲染 {k="v",...}；无标签返回空串。
func labelSuffix(labels []string, joined string) string {
	if len(labels) == 0 {
		return ""
	}
	vals := strings.Split(joined, "\x00")
	parts := make([]string, len(labels))
	for i, l := range labels {
		parts[i] = fmt.Sprintf("%s=%q", l, vals[i])
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// trimZero 桶上界浮点输出（0.001 不输出成 0.0010000000000000002）。
func trimZero(f float64) string {
	s := fmt.Sprintf("%g", f)
	if s == "+Inf" {
		return "+Inf"
	}
	return s
}
