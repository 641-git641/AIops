package external_test

import (
	"context"
	"testing"

	"aiops/internal/external"
	"aiops/internal/store"

	"gorm.io/gorm"
)

// DB 门控集成测试（PG 未运行时跳过）；store import external 会造成循环，故用外部测试包。

func openExternalDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(store.DefaultDSN, store.Options{})
	if err != nil {
		t.Skipf("postgres 未运行（docker compose），跳过: %v", err)
	}
	if err := db.AutoMigrate(&external.System{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func cleanupExternal(t *testing.T, db *gorm.DB, tenant string) {
	t.Helper()
	t.Cleanup(func() { _ = db.Exec("DELETE FROM external_systems WHERE tenant_id = ?", tenant).Error })
}

// Seed 幂等：表空播种，已有数据不覆盖（DB 为准）。
func TestSeedIdempotent(t *testing.T) {
	db := openExternalDB(t)
	tenant := "ext-seed-it"
	cleanupExternal(t, db, tenant)
	ctx := context.Background()

	svc := external.NewService(db)
	seeds := []external.SeedItem{
		{Name: "Grafana", URL: "http://localhost:3000", Type: external.TypeGrafana},
		{Name: "Bad", URL: "javascript:x", Type: external.TypeOther}, // 非法条目跳过
	}
	if err := svc.Seed(ctx, tenant, seeds); err == nil {
		t.Fatal("部分种子失败应返回错误")
	}
	items, err := svc.List(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "Grafana" {
		t.Fatalf("种子应只写入合法条目: %+v", items)
	}

	// 再次播种（即使换一批）不覆盖
	svc.Seed(ctx, tenant, []external.SeedItem{{Name: "Prometheus", URL: "http://localhost:9090", Type: external.TypePrometheus}})
	items, _ = svc.List(ctx, tenant)
	if len(items) != 1 {
		t.Fatalf("已有数据不应被再次播种: %d", len(items))
	}

	// CRUD + 更新校验
	created, err := svc.Create(ctx, tenant, external.System{Name: "日志", URL: "http://localhost:5601", Type: external.TypeLogs, Enabled: true, SortOrder: 9})
	if err != nil {
		t.Fatal(err)
	}
	upd, err := svc.Update(ctx, tenant, created.ID, external.System{Enabled: false})
	if err != nil || upd == nil || upd.Enabled {
		t.Fatalf("更新失败: %v %+v", err, upd)
	}
	if _, err := svc.Update(ctx, tenant, created.ID, external.System{URL: "javascript:x"}); err == nil {
		t.Fatal("非法 URL 更新应拒绝")
	}
	if err := svc.Delete(ctx, tenant, created.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, tenant, created.ID)
	if got != nil {
		t.Fatal("删除后不应存在")
	}
}
