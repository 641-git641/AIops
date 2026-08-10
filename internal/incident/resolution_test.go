package incident_test

import (
	"context"
	"errors"
	"testing"

	"aiops/internal/incident"
	"aiops/internal/store"

	"gorm.io/gorm"
)

func TestValidateResolution(t *testing.T) {
	valid := &incident.Resolution{Outcome: incident.OutcomeSuccess, VerificationStatus: incident.VerifVerified,
		ConfirmedRootCause: "连接池耗尽", Resolution: "重启连接池"}
	if err := incident.ValidateResolution(valid); err != nil {
		t.Fatalf("valid resolution rejected: %v", err)
	}
	cases := []*incident.Resolution{
		{Outcome: "bad", VerificationStatus: incident.VerifVerified},
		{Outcome: incident.OutcomeSuccess, VerificationStatus: "bad"},
		{Outcome: incident.OutcomeSuccess, VerificationStatus: incident.VerifVerified, ConfirmedRootCause: "", Resolution: "x"},
		{Outcome: incident.OutcomeSuccess, VerificationStatus: incident.VerifVerified, ConfirmedRootCause: "x", Resolution: ""},
	}
	for i, r := range cases {
		if err := incident.ValidateResolution(r); err == nil {
			t.Fatalf("case %d should fail: %+v", i, r)
		}
	}
	// 未验证不强制根因
	loose := &incident.Resolution{Outcome: incident.OutcomeSuccess, VerificationStatus: incident.VerifUnverified}
	if err := incident.ValidateResolution(loose); err != nil {
		t.Fatalf("unverified resolution should pass: %v", err)
	}
}

// openTestDB 连接 docker PostgreSQL（未运行则跳过——纯单测不依赖外部服务）。
func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(store.DefaultDSN, store.Options{})
	if err != nil {
		t.Skipf("postgres 未运行（docker compose），跳过集成断言: %v", err)
	}
	return db
}

func TestResolutionUpsertVersionAndHook(t *testing.T) {
	db := openTestDB(t)
	s := incident.NewService(db)
	ctx := context.Background()
	tenant := "res-test-tenant"
	incidentID := "res-test-incident"

	cleanup := func() {
		_ = db.Exec("DELETE FROM incident_resolutions WHERE tenant_id = ?", tenant).Error
	}
	cleanup()
	defer cleanup()

	var hooked bool
	var hookedVersion int64
	var hookedOp string
	s.SetResIndexHook(func(ctx context.Context, tx *gorm.DB, tid, sid string, v int64, op string) error {
		hooked, hookedVersion, hookedOp = true, v, op
		return nil
	})

	r1 := &incident.Resolution{Outcome: incident.OutcomeSuccess, VerificationStatus: incident.VerifVerified,
		ConfirmedRootCause: "连接池上限不足", Resolution: "扩容连接池"}
	if err := s.UpsertResolution(ctx, tenant, incidentID, r1); err != nil {
		t.Fatal(err)
	}
	if r1.Version != 1 || !hooked || hookedVersion != 1 || hookedOp != "upsert" {
		t.Fatalf("v1: version=%d hooked=%v hv=%d op=%s", r1.Version, hooked, hookedVersion, hookedOp)
	}
	got, err := s.GetResolution(ctx, tenant, incidentID)
	if err != nil || got == nil || got.Version != 1 {
		t.Fatalf("get after v1: %v %v", got, err)
	}

	// 更新 → version 2；ResolvedAt 保持不变
	resolvedAt := got.ResolvedAt
	hooked = false
	r2 := &incident.Resolution{Outcome: incident.OutcomePartial, VerificationStatus: incident.VerifVerified,
		ConfirmedRootCause: "连接池上限不足（二次确认）", Resolution: "扩容并调参"}
	if err := s.UpsertResolution(ctx, tenant, incidentID, r2); err != nil {
		t.Fatal(err)
	}
	if r2.Version != 2 || !hooked || hookedVersion != 2 {
		t.Fatalf("v2: version=%d hooked=%v hv=%d", r2.Version, hooked, hookedVersion)
	}
	got2, _ := s.GetResolution(ctx, tenant, incidentID)
	if got2.Version != 2 || !got2.ResolvedAt.Equal(resolvedAt) {
		t.Fatalf("v2 state: %+v", got2)
	}

	// 跨租户不可见
	if other, _ := s.GetResolution(ctx, "other-tenant", incidentID); other != nil {
		t.Fatal("cross-tenant resolution leaked")
	}

	// 钩子失败不阻塞源数据提交（reconciliation 兜底）
	s.SetResIndexHook(func(ctx context.Context, tx *gorm.DB, tid, sid string, v int64, op string) error {
		return errors.New("hook boom")
	})
	r3 := &incident.Resolution{Outcome: incident.OutcomeSuccess, VerificationStatus: incident.VerifVerified,
		ConfirmedRootCause: "x", Resolution: "y"}
	if err := s.UpsertResolution(ctx, tenant, incidentID, r3); err != nil {
		t.Fatalf("hook error must not block: %v", err)
	}
	if got3, _ := s.GetResolution(ctx, tenant, incidentID); got3.Version != 3 {
		t.Fatalf("version after hook failure = %d, want 3", got3.Version)
	}
}
