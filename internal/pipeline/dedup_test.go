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

// ---------- classifyBatchLast（批内 overlay 去重，Phase 3g） ----------

func TestClassifyBatchLast(t *testing.T) {
	// 存量 + 批内 overlay 混合：批内同 fingerprint 后序事件必须看到前序写入意图
	base := map[batchKey]*alert.LastAlert{
		{tenantID: "t1", fingerprint: "fp1"}: {AlertHash: "h1", Status: alert.StatusFiring},
	}
	cs := []batchCandidate{
		{tenant: "t1", fp: "fp1", alertHash: "h1", finalStatus: alert.StatusFiring}, // 存量 firing 同内容 → duplicate
		{tenant: "t1", fp: "fp1", alertHash: "h2", finalStatus: alert.StatusFiring}, // 内容变化 → update
		{tenant: "t1", fp: "fp2", alertHash: "h1", finalStatus: alert.StatusFiring}, // 无存量 → new
		{tenant: "t1", fp: "fp1", alertHash: "h2", finalStatus: alert.StatusFiring}, // 批内第 4 条同内容 → duplicate
	}
	kinds := classifyBatchLast(base, cs)
	want := []alert.DedupKind{alert.DedupDuplicate, alert.DedupUpdate, alert.DedupNew, alert.DedupDuplicate}
	for i, k := range kinds {
		if k != want[i] {
			t.Errorf("kinds[%d] = %q, want %q", i, k, want[i])
		}
	}
}

func TestClassifyBatchLastOverlayVisible(t *testing.T) {
	// 无存量：批内首条 new，后序同内容事件看到前序写入意图 → duplicate（禁止裸延迟 flush）
	kinds := classifyBatchLast(nil, []batchCandidate{
		{tenant: "t1", fp: "fp1", alertHash: "h1", finalStatus: alert.StatusFiring},
		{tenant: "t1", fp: "fp1", alertHash: "h1", finalStatus: alert.StatusFiring},
	})
	want := []alert.DedupKind{alert.DedupNew, alert.DedupDuplicate}
	for i, k := range kinds {
		if k != want[i] {
			t.Errorf("kinds[%d] = %q, want %q", i, k, want[i])
		}
	}
}

func TestClassifyBatchLastSuppressedOverlay(t *testing.T) {
	// 维护窗口内 suppressed 同内容 = duplicate；窗口失效后 = update（过期状态上浮）
	base := map[batchKey]*alert.LastAlert{
		{tenantID: "t1", fingerprint: "fp1"}: {AlertHash: "h1", Status: alert.StatusSuppressed},
	}
	cs := []batchCandidate{
		{tenant: "t1", fp: "fp1", alertHash: "h1", finalStatus: alert.StatusSuppressed, windowActive: true},
		{tenant: "t1", fp: "fp2", alertHash: "h1", finalStatus: alert.StatusSuppressed, windowActive: true},
		{tenant: "t1", fp: "fp2", alertHash: "h1", finalStatus: alert.StatusSuppressed, windowActive: false},
	}
	kinds := classifyBatchLast(base, cs)
	want := []alert.DedupKind{alert.DedupDuplicate, alert.DedupNew, alert.DedupUpdate}
	for i, k := range kinds {
		if k != want[i] {
			t.Errorf("kinds[%d] = %q, want %q", i, k, want[i])
		}
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
