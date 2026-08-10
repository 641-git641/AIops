package assistant

import (
	"strings"
	"testing"

	"aiops/internal/rag"
)

// ---- 注册门控：RAG/playbook 依赖缺失时工具不注册（工具列表不出现，模型无法调用） ----

func TestKnowledgeToolsGatedOnDeps(t *testing.T) {
	// 无 RAG/playbook → 3 个工具都不注册
	regNil := NewRegistry(ToolDeps{})
	for _, name := range []string{"search_knowledge", "get_similar_incidents", "list_playbooks"} {
		if _, ok := regNil.Get(name); ok {
			t.Fatalf("%s 不应在无依赖时注册", name)
		}
	}
	// 仅 RAG → 检索类注册、剧本不注册
	ragSvc := rag.NewService(nil, rag.Options{})
	regRag := NewRegistry(ToolDeps{RAG: ragSvc})
	if _, ok := regRag.Get("search_knowledge"); !ok {
		t.Fatal("search_knowledge 应注册")
	}
	if _, ok := regRag.Get("get_similar_incidents"); !ok {
		t.Fatal("get_similar_incidents 应注册")
	}
	if _, ok := regRag.Get("list_playbooks"); ok {
		t.Fatal("list_playbooks 不应在无 playbook 依赖时注册")
	}
	// 仅 playbook → 剧本注册、检索类不注册
	regPb := NewRegistry(ToolDeps{Playbooks: nil})
	if _, ok := regPb.Get("list_playbooks"); ok {
		t.Fatal("list_playbooks 不应在 nil playbook 依赖时注册")
	}
}

// ---- citationOf：来源 URL 映射（仅可定位来源生成跳转路径） ----

func TestCitationOfURLMapping(t *testing.T) {
	cases := []struct {
		srcType string
		wantURL string
	}{
		{rag.SourceIncident, "/incidents/inc-1"},
		{rag.SourcePlaybook, "/playbooks"},
		{rag.SourceRunbook, ""}, // runbook 知识源无固定路由，前端按 sourceType 兜底
	}
	for _, c := range cases {
		ev := rag.Evidence{
			SourceType: c.srcType, SourceID: "inc-1", Title: "t",
			TrustLevel: "T3",
		}
		got := citationOf(ev)
		if got.URL != c.wantURL {
			t.Fatalf("%s URL = %q, want %q", c.srcType, got.URL, c.wantURL)
		}
		if got.TrustLevel != "T3" || got.Title != "t" || got.SourceID != "inc-1" {
			t.Fatalf("字段透传异常: %+v", got)
		}
	}
}

// ---- evidenceResult：结果形状 + citation 收集 + 降级透出（纯函数，注入假包） ----

func TestEvidenceResultCollectsCitations(t *testing.T) {
	pack := &rag.EvidencePack{
		Selected: []rag.Evidence{
			{DocumentID: "d1", SourceType: rag.SourceIncident, SourceID: "inc-9",
				Title: "order 连接池耗尽", TrustLevel: "T3", Score: 0.91,
				Reasons: []string{"keyword"}, Content: "回滚配置并重启连接池" + padRune(300)},
			{DocumentID: "d2", SourceType: rag.SourcePlaybook, SourceID: "pb-1",
				Title: "rollback", TrustLevel: "T2", Score: 0.7, Reasons: []string{"exact:service"}},
		},
		DegradedReason: rag.DegradedVectorDisabled,
	}
	var citations []Citation
	got := evidenceResult(ToolCallCtx{Citations: &citations}, "q", pack)

	if len(citations) != 2 {
		t.Fatalf("citations = %d, want 2: %+v", len(citations), citations)
	}
	if citations[0].URL != "/incidents/inc-9" || citations[1].URL != "/playbooks" {
		t.Fatalf("citation URL 异常: %+v", citations)
	}
	// 降级原因透出（模型可读）
	if !strings.Contains(got, "vector_disabled") {
		t.Fatalf("结果应透出降级原因: %s", got)
	}
	if !strings.Contains(got, `"count":2`) {
		t.Fatalf("结果应含 count: %s", got)
	}
	// 内容截断 200 rune + 整体 ≤8KB
	if !strings.Contains(got, "…") {
		t.Fatalf("长内容应截断: %s", got)
	}
	if len([]rune(got)) > MaxResultBytes+500 {
		t.Fatalf("结果超裁剪预算: %d", len([]rune(got)))
	}
}

func TestEvidenceResultNoCitationsWhenNoCollector(t *testing.T) {
	pack := &rag.EvidencePack{
		Selected: []rag.Evidence{{SourceType: rag.SourceIncident, SourceID: "x", Title: "t"}},
	}
	// 无收集器不 panic
	_ = evidenceResult(ToolCallCtx{}, "q", pack)
}

func padRune(n int) string {
	return strings.Repeat("长", n)
}
