package notifythrottle

import (
	"sync"
	"testing"
	"time"
)

// newTest 注入可推进时钟的节流器。
func newTest(t *testing.T) (*Throttler, *time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	th := New(5*time.Minute, 30*time.Minute)
	th.SetNow(func() time.Time { return now })
	return th, &now
}

const (
	tenant = "default"
	fp     = "fp-1"
)

func TestFirstThenCooldown(t *testing.T) {
	th, now := newTest(t)
	d := th.Decide(tenant, fp, "磁盘告警", "high", "firing")
	if !d.Send || d.Reason != ReasonFirst {
		t.Fatalf("first = %+v, want send(first)", d)
	}
	// 冷却期内重复：抑制 + 计数
	*now = now.Add(time.Minute)
	d = th.Decide(tenant, fp, "磁盘告警", "high", "firing")
	if d.Send || d.Reason != ReasonCooldown || d.Count != 1 || !d.FirstThrottled {
		t.Fatalf("cooldown #1 = %+v", d)
	}
	d = th.Decide(tenant, fp, "磁盘告警", "high", "firing")
	if d.Send || d.Count != 2 || d.FirstThrottled {
		t.Fatalf("cooldown #2 = %+v（FirstThrottled 仅首次）", d)
	}
	// 冷却到期：等同新首条
	*now = now.Add(5 * time.Minute)
	d = th.Decide(tenant, fp, "磁盘告警", "high", "firing")
	if !d.Send || d.Reason != ReasonCooldownExpired || d.Count != 0 {
		t.Fatalf("expired = %+v", d)
	}
}

func TestUpgradeBreaksCooldown(t *testing.T) {
	th, now := newTest(t)
	th.Decide(tenant, fp, "磁盘告警", "high", "firing")
	*now = now.Add(time.Minute)
	th.Decide(tenant, fp, "磁盘告警", "high", "firing") // count=1
	*now = now.Add(time.Minute)
	d := th.Decide(tenant, fp, "磁盘告警", "critical", "firing")
	if !d.Send || d.Reason != ReasonUpgrade {
		t.Fatalf("upgrade = %+v, want send(upgrade)", d)
	}
	if d.Count != 1 {
		t.Fatalf("upgrade 必须保留积压 Count = %d, want 1", d.Count)
	}
	// 升级后仍冷却
	*now = now.Add(time.Minute)
	d = th.Decide(tenant, fp, "磁盘告警", "critical", "firing")
	if d.Send {
		t.Fatalf("post-upgrade cooldown = %+v, want suppressed", d)
	}
	// 降级不突破
	*now = now.Add(time.Minute)
	d = th.Decide(tenant, fp, "磁盘告警", "low", "firing")
	if d.Send {
		t.Fatalf("downgrade must not break cooldown: %+v", d)
	}
}

func TestResolvedClearsEntry(t *testing.T) {
	th, now := newTest(t)
	th.Decide(tenant, fp, "磁盘告警", "high", "firing")
	*now = now.Add(time.Minute)
	th.Decide(tenant, fp, "磁盘告警", "high", "firing") // count=1
	*now = now.Add(time.Minute)
	d := th.Decide(tenant, fp, "磁盘告警", "high", "resolved")
	if !d.Send || d.Reason != ReasonResolved || d.Count != 1 {
		t.Fatalf("resolved = %+v, want send(resolved) count=1", d)
	}
	// entry 已清除：再发 firing = 新 first
	d = th.Decide(tenant, fp, "磁盘告警", "high", "firing")
	if !d.Send || d.Reason != ReasonFirst {
		t.Fatalf("post-resolved firing = %+v, want new first", d)
	}
}

func TestOrphanResolvedSilent(t *testing.T) {
	th, _ := newTest(t)
	d := th.Decide(tenant, fp, "磁盘告警", "high", "resolved")
	if d.Send || d.Reason != ReasonNoActive {
		t.Fatalf("orphan resolved = %+v, want silent", d)
	}
}

func TestDueSummaries(t *testing.T) {
	th, now := newTest(t)
	th.Decide(tenant, fp, "磁盘告警", "high", "firing")
	*now = now.Add(time.Minute)
	th.Decide(tenant, fp, "磁盘告警", "high", "firing")
	*now = now.Add(time.Minute)
	th.Decide(tenant, fp, "磁盘告警", "high", "firing") // count=2
	// 未到汇总点：不返回
	if got := th.DueSummaries(); len(got) != 0 {
		t.Fatalf("pre-due summaries = %v", got)
	}
	*now = now.Add(29 * time.Minute) // 距首条 31min > 30min
	tasks := th.DueSummaries()
	if len(tasks) != 1 {
		t.Fatalf("due summaries = %d, want 1", len(tasks))
	}
	if tasks[0].Count != 2 || tasks[0].Key.Fingerprint != fp {
		t.Fatalf("summary wrong: %+v", tasks[0])
	}
	if got := SummaryText(tasks[0], 30*time.Minute); got == "" {
		t.Fatal("empty summary text")
	}
	// Count 已重置，结算点滚动：再查不到同任务
	if got := th.DueSummaries(); len(got) != 0 {
		t.Fatalf("post-reset summaries = %v", got)
	}
	// count==0 的 entry 永不出汇总
	th.Decide(tenant, "fp-2", "x", "low", "firing")
	*now = now.Add(31 * time.Minute)
	if got := th.DueSummaries(); len(got) != 0 {
		t.Fatalf("zero-count summary must not fire: %v", got)
	}
}

func TestCooldownZeroSendsAlways(t *testing.T) {
	th := New(0, 30*time.Minute) // 0 = 关闭节流
	d1 := th.Decide(tenant, fp, "x", "high", "firing")
	d2 := th.Decide(tenant, fp, "x", "high", "firing")
	if !d1.Send || !d2.Send {
		t.Fatalf("zero cooldown must send always: %+v %+v", d1, d2)
	}
}

func TestConcurrentDecide(t *testing.T) {
	th, _ := newTest(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			th.Decide(tenant, fp, "x", "high", "firing")
		}()
	}
	wg.Wait()
	// 50 并发：1 条 first + 49 条 cooldown（计数 49）
	th.mu.Lock()
	e := th.entries[Key{tenant, fp}]
	th.mu.Unlock()
	if e == nil || e.Count != 49 {
		t.Fatalf("concurrent count = %+v, want 49", e)
	}
}
