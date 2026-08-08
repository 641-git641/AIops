package pipeline

import (
	"context"
	"errors"
	"testing"

	"aiops/internal/alert"
)

// classifyLastAlert 纯函数分支覆盖（合并 2 读后基于 last_alerts 单读分类）。
func TestClassifyLastAlert(t *testing.T) {
	firing := &alert.LastAlert{AlertHash: "h1", Status: alert.StatusFiring}
	sup := &alert.LastAlert{AlertHash: "h1", Status: alert.StatusSuppressed}
	resolved := &alert.LastAlert{AlertHash: "h1", Status: alert.StatusResolved}

	cases := []struct {
		name         string
		la           *alert.LastAlert
		alertHash    string
		windowActive bool
		want         alert.DedupKind
	}{
		// firing + 同内容 → 完全重复
		{"firing same hash", firing, "h1", false, alert.DedupDuplicate},
		// firing + 内容变化 → update
		{"firing diff hash", firing, "h2", false, alert.DedupUpdate},
		// 窗口内 suppressed + 同内容 → 完全重复（避免窗口期心跳刷表）
		{"suppressed same hash in window", sup, "h1", true, alert.DedupDuplicate},
		// 窗口失效后 suppressed 同内容 → 过期状态须上浮为 firing → update
		{"suppressed same hash no window", sup, "h1", false, alert.DedupUpdate},
		// 已恢复（resolved）同内容复发 → 重新触发，非 duplicate
		{"resolved same hash", resolved, "h1", false, alert.DedupUpdate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyLastAlert(tc.la, tc.alertHash, tc.windowActive); got != tc.want {
				t.Errorf("classifyLastAlert = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyLastAlertResolvedRecurrence(t *testing.T) {
	// 回归：已恢复告警同内容复发必须重新触发（旧逻辑靠 alerts 行 status，新逻辑靠 last_alerts）
	la := &alert.LastAlert{AlertHash: "h1", Status: alert.StatusResolved}
	if got := classifyLastAlert(la, "h1", true); got != alert.DedupUpdate {
		t.Fatalf("resolved recurrence should be update, got %q", got)
	}
}

// fakeLastAlertRepo 构造可用假仓储（不触 DB），验证 Classify 只调一次 GetLastAlert。
type fakeLastAlertRepo struct {
	la    *alert.LastAlert
	err   error
	calls int
}

func (r *fakeLastAlertRepo) GetLastAlert(_ context.Context, _, _ string) (*alert.LastAlert, error) {
	r.calls++
	return r.la, r.err
}

func TestDeduperClassifySingleRead(t *testing.T) {
	repo := &fakeLastAlertRepo{la: &alert.LastAlert{AlertHash: "h1", Status: alert.StatusFiring}}
	d := NewDeduper(repo)
	kind, err := d.Classify(context.Background(), "t1", "fp", "h1", false)
	if err != nil {
		t.Fatal(err)
	}
	if kind != alert.DedupDuplicate {
		t.Fatalf("want duplicate, got %q", kind)
	}
	if repo.calls != 1 {
		t.Fatalf("Classify should read last_alerts exactly once, calls=%d", repo.calls)
	}
}

func TestDeduperClassifyNew(t *testing.T) {
	d := NewDeduper(&fakeLastAlertRepo{la: nil})
	kind, err := d.Classify(context.Background(), "t1", "fp", "h1", false)
	if err != nil {
		t.Fatal(err)
	}
	if kind != alert.DedupNew {
		t.Fatalf("no last_alert should be new, got %q", kind)
	}
}

func TestDeduperClassifyErrorPropagates(t *testing.T) {
	d := NewDeduper(&fakeLastAlertRepo{err: errors.New("db down")})
	if _, err := d.Classify(context.Background(), "t1", "fp", "h1", false); err == nil {
		t.Fatal("repo error should propagate")
	}
}
