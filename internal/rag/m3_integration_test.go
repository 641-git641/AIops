package rag_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aiops/internal/embedding"
	"aiops/internal/rag"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// M3 集成测试（KEEPON 结案即知识）：runbook 生命周期 / quarantine 复活 / 知识三态 /
// golden eval / 文档统计。模式复用 integration_test.go（DB 门控 + mock embedding + RunIndexer）。

func openM3DB(t *testing.T) (*gorm.DB, *rag.Service, context.Context) {
	t.Helper()
	db, dim := openRagTestDB(t)
	// M3 新表
	if err := db.AutoMigrate(&rag.KnowledgeSource{}, &rag.GoldenQuery{}); err != nil {
		t.Fatalf("migrate m3: %v", err)
	}
	svc := rag.NewService(db, rag.Options{
		Emb: embedding.NewMock(dim), IndexVersion: "v1",
		Interval: 100 * time.Millisecond, ReconcileEvery: time.Hour,
		RetrievalTimeout: 2 * time.Second, TopKFinal: 6, MaxContextTokens: 5000,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	go svc.RunIndexer(ctx)
	return db, svc, ctx
}

func cleanupM3(t *testing.T, db *gorm.DB) {
	t.Helper()
	cleanupIT(t, db)
	for _, tbl := range []string{"knowledge_sources", "rag_golden_queries"} {
		if err := db.Exec("DELETE FROM "+tbl+" WHERE tenant_id = ?", itTenant).Error; err != nil {
			t.Logf("cleanup %s: %v", tbl, err)
		}
	}
}

func waitSucceeded(t *testing.T, db *gorm.DB, sourceType, sourceID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var st string
		if err := db.Raw(
			`SELECT status FROM knowledge_jobs WHERE tenant_id = ? AND source_type = ? AND source_id = ?
			 ORDER BY created_at DESC LIMIT 1`, itTenant, sourceType, sourceID).Scan(&st).Error; err == nil {
			if st == "succeeded" {
				return
			}
			if st == "failed" || st == "dead" {
				var errMsg string
				_ = db.Raw(`SELECT COALESCE(last_error,'') FROM knowledge_jobs WHERE tenant_id = ? AND source_type = ? AND source_id = ? ORDER BY created_at DESC LIMIT 1`,
					itTenant, sourceType, sourceID).Scan(&errMsg).Error
				t.Fatalf("job failed: %s", errMsg)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("job not succeeded within timeout")
}

func srcContent(name string, v int) string {
	return "---\ntitle: " + name + "\nservice: pay\ntags: [mysql, pool]\n---\n连接池排障步骤 v" + string(rune('0'+v)) + "\n1. 查看连接数\n2. 重启池"
}

func TestRunbookLifecycle(t *testing.T) {
	db, svc, _ := openM3DB(t)
	defer cleanupM3(t, db)
	ctx := context.Background()

	// draft：无 job 无文档
	src, err := svc.CreateSource(ctx, itTenant, rag.SourceInput{Name: "pool.md", Content: srcContent("连接池", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if src.Status != rag.SrcDraft || src.Version != 0 {
		t.Fatalf("draft wrong: %+v", src)
	}
	var jobs int64
	_ = db.Model(&rag.KnowledgeJob{}).Where("tenant_id = ? AND source_type = 'runbook'", itTenant).Count(&jobs).Scan(&jobs)
	if jobs != 0 {
		t.Fatalf("draft must not enqueue, jobs=%d", jobs)
	}
	// 发布 → 索引 → active v1 → 检索命中
	if _, err := svc.PublishSource(ctx, itTenant, src.ID); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, db, rag.SourceRunbook, src.ID)
	var doc rag.KnowledgeDocument
	if err := db.Where("tenant_id = ? AND source_type = 'runbook' AND source_id = ?", itTenant, src.ID).First(&doc).Error; err != nil {
		t.Fatal(err)
	}
	if doc.Status != rag.DocActive || doc.SourceVersion != 1 || doc.TrustLevel != rag.TrustT2 {
		t.Fatalf("doc wrong: %+v", doc)
	}
	pack := svc.Retrieve(ctx, rag.RetrievalQuery{TenantID: itTenant, Summary: "连接池耗尽 排障"})
	if !anyDoc(pack.Candidates, doc.ID) {
		t.Fatalf("runbook not retrieved: %+v", pack.Candidates)
	}
	// 更新（published 内容变化）→ v2 自动发布，v1 superseded
	if _, err := svc.UpdateSource(ctx, itTenant, src.ID, rag.SourceInput{Name: "pool.md", Content: srcContent("连接池", 2)}); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, db, rag.SourceRunbook, src.ID)
	var v1, v2 rag.KnowledgeDocument
	db.Where("tenant_id = ? AND source_type = 'runbook' AND source_id = ? AND source_version = 1", itTenant, src.ID).First(&v1)
	db.Where("tenant_id = ? AND source_type = 'runbook' AND source_id = ? AND source_version = 2", itTenant, src.ID).First(&v2)
	if v1.Status != rag.DocSuperseded || v2.Status != rag.DocActive {
		t.Fatalf("version supersede wrong: v1=%s v2=%s", v1.Status, v2.Status)
	}
	// 删除 → 文档 deleted + chunks/embeddings 清零 + 检索不命中
	if err := svc.DeleteSource(ctx, itTenant, src.ID); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, db, rag.SourceRunbook, src.ID)
	var delDoc rag.KnowledgeDocument
	db.Where("tenant_id = ? AND source_type = 'runbook' AND source_id = ? AND status = 'active'", itTenant, src.ID).First(&delDoc)
	if delDoc.ID != "" {
		t.Fatal("active doc must be removed after delete")
	}
	var chunks, embs int64
	db.Raw(`SELECT count(*) FROM knowledge_chunks WHERE document_id = ?`, doc.ID).Scan(&chunks)
	db.Raw(`SELECT count(*) FROM knowledge_embeddings WHERE chunk_id IN (SELECT id FROM knowledge_chunks WHERE document_id = ?)`, doc.ID).Scan(&embs)
	if chunks != 0 || embs != 0 {
		t.Fatalf("delete propagation wrong: chunks=%d embs=%d", chunks, embs)
	}
	pack2 := svc.Retrieve(ctx, rag.RetrievalQuery{TenantID: itTenant, Summary: "连接池耗尽 排障"})
	if anyDoc(pack2.Candidates, doc.ID) {
		t.Fatal("deleted runbook must not be retrieved")
	}
}

func TestRunbookQuarantineAndRepublish(t *testing.T) {
	db, svc, _ := openM3DB(t)
	defer cleanupM3(t, db)
	ctx := context.Background()

	src, _ := svc.CreateSource(ctx, itTenant, rag.SourceInput{Name: "q.md", Content: srcContent("隔离测试", 1)})
	if _, err := svc.PublishSource(ctx, itTenant, src.ID); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, db, rag.SourceRunbook, src.ID)
	var doc rag.KnowledgeDocument
	db.Where("tenant_id = ? AND source_type = 'runbook' AND source_id = ?", itTenant, src.ID).First(&doc)
	// unsafe 反馈 → 文档 quarantined + source 联动
	if _, err := svc.AddFeedback(ctx, itTenant, "inc-1", &rag.RAGFeedback{
		Rating: rag.RatingUnsafe, DocumentID: doc.ID,
	}); err != nil {
		t.Fatal(err)
	}
	var src2 rag.KnowledgeSource
	db.Where("tenant_id = ? AND id = ?", itTenant, src.ID).First(&src2)
	if src2.Status != rag.SrcQuarantined {
		t.Fatalf("source not synced to quarantined: %s", src2.Status)
	}
	// 重新发布 → 新版本天然复活 + source 回 published
	if _, err := svc.PublishSource(ctx, itTenant, src.ID); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, db, rag.SourceRunbook, src.ID)
	var newDoc rag.KnowledgeDocument
	db.Where("tenant_id = ? AND source_type = 'runbook' AND source_id = ? AND status = 'active'", itTenant, src.ID).First(&newDoc)
	if newDoc.ID == "" || newDoc.SourceVersion != 2 {
		t.Fatalf("republish must create new active version: %+v", newDoc)
	}
	db.Where("tenant_id = ? AND id = ?", itTenant, src.ID).First(&src2)
	if src2.Status != rag.SrcPublished {
		t.Fatalf("source not back to published: %s", src2.Status)
	}
}

func TestKnowledgeStatusThreeStates(t *testing.T) {
	db, svc, _ := openM3DB(t)
	defer cleanupM3(t, db)
	ctx := context.Background()

	// 未解决 → 三态全 false（seedIncident 建的是已解决事故，这里手动插 open 态）
	if err := db.Exec(`INSERT INTO incidents (id, tenant_id, name, summary, severity, status, source, created_at)
		VALUES ('ks-open', ?, '连接池耗尽', 'order api latency db_pool_exhausted', 'critical', 'open', 'agent', now())`,
		itTenant).Error; err != nil {
		t.Fatal(err)
	}
	st := svc.KnowledgeStatus(ctx, itTenant, "ks-open")
	if st.Resolved || st.ResolutionFilled || st.Indexed {
		t.Fatalf("open incident must be all-false: %+v", st)
	}
	// 已解决未结案（resolve 触发 version=0 任务 → T1 参考文档）
	if err := db.Exec(`UPDATE incidents SET status = 'resolved', resolved_at = now() WHERE id = 'ks-open'`).Error; err != nil {
		t.Fatal(err)
	}
	_ = rag.EnqueueJob(ctx, db, itTenant, rag.SourceIncident, "ks-open", 0, rag.OpUpsert)
	waitSucceeded(t, db, rag.SourceIncident, "ks-open")
	st = svc.KnowledgeStatus(ctx, itTenant, "ks-open")
	if !st.Resolved || st.ResolutionFilled || !st.Indexed {
		t.Fatalf("resolved-no-resolution state wrong: %+v", st)
	}
	// 结案（verified → v1）→ indexed + T3
	if err := db.Exec(`INSERT INTO incident_resolutions (id, tenant_id, incident_id, confirmed_root_cause, resolution,
		outcome, verification_status, version, updated_at, resolved_at)
		VALUES ('res-ks-open', ?, 'ks-open', '连接池上限配置不匹配', '回滚并重启', 'success', 'verified', 1, now(), now())`,
		itTenant).Error; err != nil {
		t.Fatal(err)
	}
	_ = rag.EnqueueJob(ctx, db, itTenant, rag.SourceIncident, "ks-open", 1, rag.OpUpsert)
	waitSucceeded(t, db, rag.SourceIncident, "ks-open")
	st = svc.KnowledgeStatus(ctx, itTenant, "ks-open")
	if !st.Indexed || st.Document == nil || st.Document.TrustLevel != rag.TrustT3 || st.Document.SourceVersion != 1 {
		t.Fatalf("indexed state wrong: %+v", st)
	}
	// rejected 结案 → 文档被删除传播 → indexed=false（seedIncident 自带 verified resolution，改为 rejected）
	seedIncident(t, db, "ks-rej")
	if err := db.Exec(`UPDATE incident_resolutions SET verification_status = 'rejected' WHERE incident_id = 'ks-rej'`).Error; err != nil {
		t.Fatal(err)
	}
	_ = rag.EnqueueJob(ctx, db, itTenant, rag.SourceIncident, "ks-rej", 1, rag.OpUpsert)
	waitSucceeded(t, db, rag.SourceIncident, "ks-rej")
	st = svc.KnowledgeStatus(ctx, itTenant, "ks-rej")
	if st.Indexed {
		t.Fatalf("rejected must not index: %+v", st)
	}
}

func TestGoldenQueryCRUDAndEval(t *testing.T) {
	db, svc, _ := openM3DB(t)
	defer cleanupM3(t, db)
	ctx := context.Background()

	// 造两个知识源：事故（seedIncident + resolution）+ runbook
	seedIncident(t, db, "gold-inc")
	_ = rag.EnqueueJob(ctx, db, itTenant, rag.SourceIncident, "gold-inc", 1, rag.OpUpsert)
	src, _ := svc.CreateSource(ctx, itTenant, rag.SourceInput{Name: "gold.md", Content: srcContent("金标", 1)})
	_, _ = svc.PublishSource(ctx, itTenant, src.ID)
	waitSucceeded(t, db, rag.SourceIncident, "gold-inc")
	waitSucceeded(t, db, rag.SourceRunbook, src.ID)

	// golden CRUD
	exp, _ := json.Marshal([]string{"gold-inc"})
	g := &rag.GoldenQuery{Query: "连接池耗尽", ExpectedSources: datatypes.JSON(exp), Enabled: true}
	if err := svc.CreateGoldenQuery(ctx, itTenant, g); err != nil {
		t.Fatal(err)
	}
	if g.ID == "" {
		t.Fatal("id must be assigned")
	}
	// 不存在的 source → 校验失败
	bad, _ := json.Marshal([]string{"no-such-source"})
	if err := svc.CreateGoldenQuery(ctx, itTenant, &rag.GoldenQuery{Query: "x", ExpectedSources: datatypes.JSON(bad)}); err == nil {
		t.Fatal("nonexistent expected source must fail")
	}

	// RunEval：查询命中事故 + runbook（expected 里加 runbook source）
	exp2, _ := json.Marshal([]string{"gold-inc", src.ID})
	g2 := &rag.GoldenQuery{Query: "连接池 排障", ExpectedSources: datatypes.JSON(exp2), Enabled: true}
	if err := svc.CreateGoldenQuery(ctx, itTenant, g2); err != nil {
		t.Fatal(err)
	}
	m, err := svc.RunEval(ctx, rag.EvalOptions{Tenants: []string{itTenant}, EnabledOnly: true,
		TopKFinal: 6, MaxContextTokens: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 2 {
		t.Fatalf("total = %d, want 2", m.Total)
	}
	if m.AnyHitRate != 1.0 {
		t.Fatalf("anyHitRate = %.2f, want 1.0（两查询都应收敛到来源）", m.AnyHitRate)
	}
	if m.Params.TopKFinal != 6 {
		t.Fatalf("params wrong: %+v", m.Params)
	}
}

func TestDocStats(t *testing.T) {
	db, svc, _ := openM3DB(t)
	defer cleanupM3(t, db)
	ctx := context.Background()

	// 造多种状态文档
	insert := func(status, srcType, srcID string, ver int64) {
		if err := db.Exec(`INSERT INTO knowledge_documents (id, tenant_id, source_type, source_id, source_version,
			title, body, trust_level, verification_status, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 't', 'b', 'T2', 'verified', ?, now(), now())`,
			srcType+"-"+srcID+"-"+string(rune('0'+ver)), itTenant, srcType, srcID, ver, status).Error; err != nil {
			t.Fatal(err)
		}
	}
	// 全库统计（无租户过滤——rag/status 是全局视角）：用增量断言（真实库可能已有文档）
	before := svc.Status(ctx).Documents
	insert(rag.DocActive, rag.SourceRunbook, "a", 1)
	insert(rag.DocActive, rag.SourceIncident, "b", 2)
	insert(rag.DocSuperseded, rag.SourceRunbook, "a", 0)
	insert(rag.DocQuarantined, rag.SourceRunbook, "c", 1)
	info := svc.Status(ctx)
	got := info.Documents
	if got.Total-before.Total != 4 || got.Active-before.Active != 2 ||
		got.Superseded-before.Superseded != 1 || got.Quarantined-before.Quarantined != 1 {
		t.Fatalf("doc stats delta wrong: before=%+v after=%+v", before, got)
	}
	// BySourceType 无法可靠增量（真实库可能已有 runbook/incident 分布）→ 只断言总分布键存在
	if got.BySourceType == nil {
		t.Fatal("bySourceType missing")
	}
}

// anyDoc 候选是否包含目标文档。
func anyDoc(evs []rag.Evidence, docID string) bool {
	for _, e := range evs {
		if e.DocumentID == docID {
			return true
		}
	}
	return false
}
