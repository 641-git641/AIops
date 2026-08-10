package assistant_test

import (
	"context"
	"testing"
	"time"

	"aiops/internal/assistant"
)

func TestBrokerPublishSequenceAndSubscribe(t *testing.T) {
	db := openDB(t)
	tenant := "asst-stream"
	cleanup(t, db, tenant)
	ctx := context.Background()

	broker := assistant.NewBroker(db)
	ch, unsub := broker.Subscribe("run-1")
	defer unsub()

	seq1, err := broker.Publish(ctx, "run-1", tenant, "run.started", map[string]any{"status": "running"})
	if err != nil {
		t.Fatal(err)
	}
	seq2, err := broker.Publish(ctx, "run-1", tenant, "message.delta", map[string]any{"content": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if seq2 != seq1+1 {
		t.Fatalf("sequence 不连续: %d -> %d", seq1, seq2)
	}
	// 实时订阅收到两条
	ev1, ev2 := <-ch, <-ch
	if ev1.Type != "run.started" || ev2.Type != "message.delta" || ev2.Sequence != seq2 {
		t.Fatalf("events = %+v / %+v", ev1, ev2)
	}
	// DB 快照（lastSeq=seq1 → 只返回第二条）
	snapshot, err := broker.EventsSince(ctx, tenant, "run-1", seq1)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].Type != "message.delta" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	// 跨租户读不到
	if cross, _ := broker.EventsSince(ctx, "other", "run-1", 0); len(cross) != 0 {
		t.Fatalf("跨租户读到事件: %+v", cross)
	}
	// 退订后不再收到
	unsub()
	select {
	case <-ch:
		t.Fatal("退订后仍收到事件")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBrokerSequencePerRunIndependent(t *testing.T) {
	db := openDB(t)
	tenant := "asst-stream2"
	cleanup(t, db, tenant)
	ctx := context.Background()

	broker := assistant.NewBroker(db)
	s1, _ := broker.Publish(ctx, "run-a", tenant, "run.started", nil)
	s2, _ := broker.Publish(ctx, "run-b", tenant, "run.started", nil)
	if s1 != 1 || s2 != 1 {
		t.Fatalf("各 run sequence 应独立: %d / %d", s1, s2)
	}
}
