// Package notifythrottle 实现告警通知节流（KEEPON 5.4）：同 fingerprint 首条立即通知，
// 冷却期内重复合并（按周期发汇总），严重度升级/恢复突破冷却立即通知。
// 纯内存状态（UX 层优化——告警数据在 DB，重启丢失仅导致同指纹首条重发一次，审计可解释）。
package notifythrottle

import (
	"strconv"
	"sync"
	"time"

	"aiops/internal/alert"
)

// Reason 通知决策原因（写审计，可解释"为何发送/为何被节流"）。
const (
	ReasonFirst           = "first"            // 首条告警，立即通知
	ReasonUpgrade         = "upgrade"          // 严重度升级，突破冷却
	ReasonResolved        = "resolved"         // 恢复，单独通知并清空冷却
	ReasonCooldownExpired = "cooldown-expired" // 冷却到期后事件再来，重置周期
	ReasonCooldown        = "cooldown"         // 冷却期内，合并通知
	ReasonNoActive        = "no-active"        // 无活跃条目（孤儿 resolved 等），不发送
)

// Key 节流维度：tenant + fingerprint（KEEPON：至少包含 tenant、channel、fingerprint；
// 通知渠道是全局配置，此处按 fingerprint 维度即可，渠道维度由发送时统一生效）。
type Key struct {
	TenantID    string
	Fingerprint string
}

// Entry 单个指纹的节流状态。
type Entry struct {
	Name          string
	Severity      string
	Rank          int
	FirstAt       time.Time // 本周期首条时间
	LastAt        time.Time // 最近一次事件时间
	Count         int       // 冷却期内累计次数（首条不计入）
	CooldownUntil time.Time
	SummaryAt     time.Time // 下次汇总结算点
}

// Decision 单条事件的通知决策。
type Decision struct {
	Send           bool
	Reason         string
	Count          int       // 冷却期内累计（upgrade 保留积压）
	FirstThrottled bool      // 冷却期内首次被抑制（审计只记这一条）
	Since, Until   time.Time // 汇总窗口（首条/最近事件）
}

// SummaryTask 到期的汇总任务（锁内快照，锁外发送）。
type SummaryTask struct {
	Key      Key
	Name     string
	Severity string
	Count    int
	FirstAt  time.Time
	LastAt   time.Time
}

// Throttler 节流状态机（并发安全；now 可注入时钟供单测）。
type Throttler struct {
	mu              sync.Mutex
	entries         map[Key]*Entry
	Cooldown        time.Duration
	SummaryInterval time.Duration // 0 = 关闭汇总
	now             func() time.Time
}

// New 创建节流器（cooldown <= 0 时每事件都发，语义=关闭节流）。
func New(cooldown, summaryInterval time.Duration) *Throttler {
	return &Throttler{
		entries:         make(map[Key]*Entry),
		Cooldown:        cooldown,
		SummaryInterval: summaryInterval,
		now:             time.Now,
	}
}

// SetNow 注入时钟（单测用）。
func (t *Throttler) SetNow(fn func() time.Time) { t.now = fn }

// Decide 对一条告警事件做通知决策（纯内存，无 IO）。
func (t *Throttler) Decide(tenantID, fingerprint, name, severity, status string) Decision {
	now := t.now()
	key := Key{TenantID: tenantID, Fingerprint: fingerprint}
	rank := alert.SeverityRank(severity)
	resolved := status == alert.StatusResolved

	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.entries[key]
	if !ok {
		if resolved {
			return Decision{Reason: ReasonNoActive} // 孤儿 resolved（内存态重启后）不发送
		}
		t.entries[key] = &Entry{
			Name: name, Severity: severity, Rank: rank,
			FirstAt: now, LastAt: now, Count: 0,
			CooldownUntil: now.Add(t.Cooldown),
			SummaryAt:     now.Add(t.SummaryInterval),
		}
		return Decision{Send: true, Reason: ReasonFirst, Since: now, Until: now}
	}

	if resolved {
		delete(t.entries, key) // 恢复：发送 + 清空冷却与未到点汇总
		return Decision{Send: true, Reason: ReasonResolved, Count: entry.Count, Since: entry.FirstAt, Until: now}
	}

	entry.Name = name
	entry.LastAt = now

	// 严重度升级突破冷却（保留 Count：汇总仍报告升级前的积压）
	if rank > entry.Rank {
		entry.Rank = rank
		entry.Severity = severity
		entry.CooldownUntil = now.Add(t.Cooldown)
		return Decision{Send: true, Reason: ReasonUpgrade, Count: entry.Count, Since: entry.FirstAt, Until: now}
	}

	// 冷却到期：等同新周期首条（重置周期；Count 归零——旧积压已由汇总结算）
	if !now.Before(entry.CooldownUntil) {
		entry.FirstAt = now
		entry.Count = 0
		entry.CooldownUntil = now.Add(t.Cooldown)
		entry.SummaryAt = now.Add(t.SummaryInterval)
		return Decision{Send: true, Reason: ReasonCooldownExpired, Since: now, Until: now}
	}

	// 冷却期内：合并（首条被抑制才需要审计）
	entry.Count++
	first := entry.Count == 1
	return Decision{
		Send: false, Reason: ReasonCooldown, Count: entry.Count,
		FirstThrottled: first, Since: entry.FirstAt, Until: now,
	}
}

// DueSummaries 收集到期的汇总任务并重置结算点（锁内快照，调用方锁外发送）。
func (t *Throttler) DueSummaries() []SummaryTask {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []SummaryTask
	for key, e := range t.entries {
		if e.Count == 0 || now.Before(e.SummaryAt) {
			continue
		}
		out = append(out, SummaryTask{
			Key: key, Name: e.Name, Severity: e.Severity,
			Count: e.Count, FirstAt: e.FirstAt, LastAt: e.LastAt,
		})
		e.Count = 0
		e.SummaryAt = now.Add(t.SummaryInterval)
	}
	return out
}

// SummaryText 汇总文案（"过去 30 分钟重复 47 次"）。
func SummaryText(task SummaryTask, interval time.Duration) string {
	return "「" + task.Name + "」在过去 " + formatDuration(interval) + " 内重复 " +
		strconv.Itoa(task.Count) + " 次（首次 " + task.FirstAt.Format("15:04:05") +
		"，最近 " + task.LastAt.Format("15:04:05") + "，当前严重度 " + task.Severity + "）"
}

func formatDuration(d time.Duration) string {
	if d >= time.Hour {
		return strconv.Itoa(int(d.Hours())) + " 小时"
	}
	return strconv.Itoa(int(d.Minutes())) + " 分钟"
}
