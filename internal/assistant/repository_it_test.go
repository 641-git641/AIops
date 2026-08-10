package assistant_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"aiops/internal/assistant"
	"aiops/internal/store"
	"aiops/internal/util"

	"gorm.io/gorm"
)

// openDB PostgreSQL 可用则连库，否则跳过（沿用 rag/overview 集成测试模式）。
func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(store.DefaultDSN, store.Options{})
	if err != nil {
		t.Skipf("postgres 未运行（docker compose），跳过: %v", err)
	}
	// 各测试独立租户前缀，Cleanup 删除
	return db
}

func cleanup(t *testing.T, db *gorm.DB, tenant string) {
	t.Helper()
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM assistant_events WHERE tenant_id = ?", tenant).Error
		_ = db.Exec("DELETE FROM assistant_steps WHERE tenant_id = ?", tenant).Error
		_ = db.Exec("DELETE FROM assistant_messages WHERE tenant_id = ?", tenant).Error
		_ = db.Exec("DELETE FROM assistant_action_requests WHERE tenant_id = ?", tenant).Error
		_ = db.Exec("DELETE FROM assistant_runs WHERE tenant_id = ?", tenant).Error
		_ = db.Exec("DELETE FROM assistant_threads WHERE tenant_id = ?", tenant).Error
	})
}

func newThread(t *testing.T, db *gorm.DB, tenant string) *assistant.AssistantThread {
	t.Helper()
	th := &assistant.AssistantThread{
		ID: util.NewID(), TenantID: tenant, Title: "t", Status: assistant.ThreadActive,
		CreatedBy: "test", CreatedAt: time.Now().UTC(),
	}
	if err := assistant.CreateThread(context.Background(), db, th); err != nil {
		t.Fatal(err)
	}
	return th
}

func newRun(t *testing.T, db *gorm.DB, tenant, threadID string) *assistant.AssistantRun {
	t.Helper()
	run := &assistant.AssistantRun{
		ID: util.NewID(), TenantID: tenant, ThreadID: threadID,
		Model: "mock", PromptVersion: "v1", IdempotencyKey: util.NewID(),
	}
	id, err := assistant.EnqueueRun(context.Background(), db, run)
	if err != nil {
		t.Fatal(err)
	}
	run.ID = id
	return run
}

func TestEnqueueRunIdempotent(t *testing.T) {
	db := openDB(t)
	tenant := "asst-idem"
	cleanup(t, db, tenant)
	cleanup(t, db, "asst-idem-other") // 本测试还会写入另一租户
	ctx := context.Background()

	run := &assistant.AssistantRun{
		ID: util.NewID(), TenantID: tenant, ThreadID: "th1",
		Model: "mock", PromptVersion: "v1", IdempotencyKey: "key-1",
	}
	id1, err := assistant.EnqueueRun(ctx, db, run)
	if err != nil {
		t.Fatal(err)
	}
	// 同 idempotency_key 二次入队 → 返回既有 id（不产生新行）
	dup := &assistant.AssistantRun{
		ID: util.NewID(), TenantID: tenant, ThreadID: "th1",
		Model: "mock", PromptVersion: "v1", IdempotencyKey: "key-1",
	}
	id2, err := assistant.EnqueueRun(ctx, db, dup)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("idempotency 失效：%s != %s", id1, id2)
	}
	var n int64
	if err := db.Raw(`SELECT count(*) FROM assistant_runs WHERE tenant_id = ? AND idempotency_key = 'key-1'`, tenant).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
	// 不同租户同 key 允许（租户隔离）
	run2 := &assistant.AssistantRun{
		ID: util.NewID(), TenantID: "asst-idem-other", ThreadID: "th1",
		Model: "mock", PromptVersion: "v1", IdempotencyKey: "key-1",
	}
	if _, err := assistant.EnqueueRun(ctx, db, run2); err != nil {
		t.Fatal(err)
	}
}

func TestClaimRunsLeaseAndRevive(t *testing.T) {
	db := openDB(t)
	tenant := "asst-lease"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	run := newRun(t, db, tenant, th.ID)

	// 单 worker 领取
	claims, err := assistant.ClaimRuns(ctx, db, "worker-a", tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].ID != run.ID {
		t.Fatalf("claims = %+v", claims)
	}
	if claims[0].Attempts != 1 || claims[0].LockedBy != "worker-a" {
		t.Fatalf("claim state = %+v", claims[0])
	}

	// running 未超租约 → 第二个 worker 领不到
	claims2, _ := assistant.ClaimRuns(ctx, db, "worker-b", tenant, 10)
	if len(claims2) != 0 {
		t.Fatalf("running 未超租约被重复领取: %+v", claims2)
	}

	// 租约过期 → 复活（直接改 locked_at 模拟崩溃）
	if err := db.Exec(`UPDATE assistant_runs SET locked_at = now() - make_interval(secs => 300) WHERE id = ?`, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	claims3, err := assistant.ClaimRuns(ctx, db, "worker-b", tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims3) != 1 || claims3[0].ID != run.ID || claims3[0].Attempts != 2 {
		t.Fatalf("revive = %+v", claims3)
	}
}

func TestClaimRunsConcurrentNoDup(t *testing.T) {
	db := openDB(t)
	tenant := "asst-conc"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	for i := 0; i < 5; i++ {
		newRun(t, db, tenant, th.ID)
	}
	// 两个 worker 并发领取 5 个 run，无重复
	var mu sync.Mutex
	claimed := map[string]string{} // runID → worker
	var wg sync.WaitGroup
	for _, w := range []string{"w1", "w2"} {
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				runs, err := assistant.ClaimRuns(ctx, db, worker, tenant, 10)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				mu.Lock()
				for _, r := range runs {
					if prev, ok := claimed[r.ID]; ok {
						t.Errorf("run %s 被 %s 与 %s 重复领取", r.ID, prev, worker)
					}
					claimed[r.ID] = worker
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(claimed) != 5 {
		t.Fatalf("claimed = %d, want 5", len(claimed))
	}
}

func TestCancelRunStates(t *testing.T) {
	db := openDB(t)
	tenant := "asst-cancel"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	run := newRun(t, db, tenant, th.ID)

	// queued 可取消
	if err := assistant.CancelRun(ctx, db, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := assistant.GetRun(ctx, db, tenant, run.ID)
	if got.Status != assistant.RunCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
	// 终态不可再取消（无影响）
	if err := assistant.CancelRun(ctx, db, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	// 跨租户不可取消（WHERE tenant 强制）
	if err := assistant.CancelRun(ctx, db, "other-tenant", run.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = assistant.GetRun(ctx, db, tenant, run.ID)
	if got.Status != assistant.RunCancelled {
		t.Fatalf("跨租户 cancel 篡改了状态: %s", got.Status)
	}
}

func TestFailRunBackoffAndDead(t *testing.T) {
	db := openDB(t)
	tenant := "asst-fail"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	run := newRun(t, db, tenant, th.ID)

	for i := 0; i < 8; i++ { // 8 = runMaxAttempts（包内常量，外部测试用字面量）
		if err := assistant.FailRun(ctx, db, run.ID, i+1, testErr("boom")); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := assistant.GetRun(ctx, db, tenant, run.ID)
	if got.Status != "dead" {
		t.Fatalf("status = %s, want dead（8 次失败）", got.Status)
	}
	// dead 后可重新入队（幂等复活——对齐 rag EnqueueJob 语义，此处人工重试）
	var n int64
	db.Raw(`SELECT count(*) FROM assistant_runs WHERE id = ? AND status = 'dead'`, run.ID).Scan(&n)
	if n != 1 {
		t.Fatal("dead run 应存在")
	}
}

func testErr(s string) error { return &errStr{s} }

type errStr struct{ s string }

func (e *errStr) Error() string { return e.s }

func TestMessageSequenceAndTenantIsolation(t *testing.T) {
	db := openDB(t)
	tenant := "asst-msg"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	seq1, err := assistant.NextMessageSequence(ctx, db, tenant, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	m1 := &assistant.AssistantMessage{ID: util.NewID(), TenantID: tenant, ThreadID: th.ID,
		Role: assistant.MsgUser, Content: "hi", Sequence: seq1}
	if err := assistant.CreateMessage(ctx, db, m1); err != nil {
		t.Fatal(err)
	}
	seq2, _ := assistant.NextMessageSequence(ctx, db, tenant, th.ID)
	if seq2 != seq1+1 {
		t.Fatalf("seq2 = %d, want %d", seq2, seq1+1)
	}
	// 跨租户读不到消息
	msgs, err := assistant.ListMessages(ctx, db, "other-tenant", th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("跨租户读到消息: %+v", msgs)
	}
	// 跨租户 thread 查不到
	if th2, _ := assistant.GetThread(ctx, db, "other-tenant", th.ID); th2 != nil {
		t.Fatal("跨租户读到 thread")
	}
}

func TestThreadSoftDelete(t *testing.T) {
	db := openDB(t)
	tenant := "asst-del"
	cleanup(t, db, tenant)
	ctx := context.Background()

	th := newThread(t, db, tenant)
	if err := assistant.SoftDeleteThread(ctx, db, tenant, th.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := assistant.GetThread(ctx, db, tenant, th.ID); got != nil {
		t.Fatal("软删后仍可查询")
	}
	// 列表不含软删
	threads, _ := assistant.ListThreads(ctx, db, tenant, 10)
	for _, x := range threads {
		if x.ID == th.ID {
			t.Fatal("列表含软删 thread")
		}
	}
}
