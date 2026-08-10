package auth_test

import (
	"context"
	"testing"

	"aiops/internal/auth"
	"aiops/internal/store"

	"gorm.io/gorm"
)

// openTestDB PostgreSQL 可用则连库，否则跳过（沿用 rag 集成测试模式）。
func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(store.DefaultDSN, store.Options{})
	if err != nil {
		t.Skipf("postgres 未运行（docker compose），跳过集成断言: %v", err)
	}
	if err := db.AutoMigrate(&auth.APIKey{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Exec("DELETE FROM api_keys").Error })
	return db
}

func TestCreateVerifyDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := auth.NewService(db)

	secret, err := s.Create(ctx, "default", "script-a")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := s.Verify(ctx, secret)
	if err != nil || tenant != "default" {
		t.Fatalf("verify = %q, %v", tenant, err)
	}
	// 撤销后立即失效
	keys, _ := s.List(ctx, "default")
	if len(keys) != 1 {
		t.Fatalf("list = %d, want 1", len(keys))
	}
	if err := s.Delete(ctx, "default", keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, secret); err == nil {
		t.Fatal("verify after delete must fail")
	}
	// 跨租户不可删
	secret2, _ := s.Create(ctx, "other", "k")
	keys, _ = s.List(ctx, "other")
	if err := s.Delete(ctx, "default", keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, secret2); err != nil {
		t.Fatal("other tenant key must survive")
	}
}

func TestEnsureOwnerKey(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := auth.NewService(db)

	owner, err := s.EnsureOwnerKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if owner == "" {
		t.Fatal("empty owner key on empty table")
	}
	// 幂等：已有 key 不再重复生成
	owner2, err := s.EnsureOwnerKey(ctx)
	if err != nil || owner2 != "" {
		t.Fatalf("second ensure = %q, %v (want empty)", owner2, err)
	}
	// owner 标记
	keys, _ := s.List(ctx, auth.DefaultTenant)
	if len(keys) != 1 || !keys[0].IsOwner {
		t.Fatalf("owner key missing: %+v", keys)
	}
	// regenerate：旧 key 失效，新 key 有效且为 owner
	newOwner, err := s.RegenerateOwner(ctx, auth.DefaultTenant)
	if err != nil || newOwner == "" {
		t.Fatalf("regenerate = %q, %v", newOwner, err)
	}
	if _, err := s.Verify(ctx, owner); err == nil {
		t.Fatal("old owner key must be revoked")
	}
	if _, err := s.Verify(ctx, newOwner); err != nil {
		t.Fatal("new owner key must verify")
	}
}
