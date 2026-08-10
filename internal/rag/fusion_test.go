package rag

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mkEv(chunk, doc, sourceType, service, trust string, t time.Time) Evidence {
	return Evidence{
		ChunkID: chunk, DocumentID: doc, SourceType: sourceType, SourceID: doc,
		Service: service, TrustLevel: trust, Time: t, Content: "内容",
		Reasons: []string{"keyword"},
	}
}

func TestFuseRRFOrdering(t *testing.T) {
	now := time.Now()
	// 三路候选：a 在 keyword 第 1、vector 第 3；b 在 keyword 第 2、vector 第 1
	lanes := map[string][]Evidence{
		"keyword": {
			mkEv("a", "d1", SourceIncident, "order", TrustT1, now),
			mkEv("b", "d2", SourceIncident, "pay", TrustT1, now),
		},
		"vector": {
			mkEv("b", "d2", SourceIncident, "pay", TrustT1, now),
			mkEv("c", "d3", SourceIncident, "search", TrustT1, now),
			mkEv("a", "d1", SourceIncident, "order", TrustT1, now),
		},
	}
	out := fuse(lanes, RetrievalQuery{}, 6, 2)
	if len(out) == 0 {
		t.Fatal("no fused results")
	}
	// b: 1/61 + 1/61 = 0.03279; a: 1/62 + 1/63 = 0.03202 → b 应排第一
	if out[0].ChunkID != "b" {
		t.Fatalf("top = %s, want b (RRF 计算)", out[0].ChunkID)
	}
	if out[0].Score <= out[1].Score {
		t.Fatalf("scores not descending: %v", out)
	}
}

func TestFuseBusinessWeights(t *testing.T) {
	now := time.Now()
	// 同服务 T3 → 应超过不同服务 T1（即使 RRF 分略低）
	lanes := map[string][]Evidence{
		"keyword": {
			mkEv("x", "dx", SourceIncident, "other", TrustT1, now),
			mkEv("y", "dy", SourceIncident, "order", TrustT3, now),
		},
	}
	out := fuse(lanes, RetrievalQuery{Service: "order"}, 6, 2)
	if out[0].ChunkID != "y" {
		t.Fatalf("top = %s, want y（同服务+T3 加权）", out[0].ChunkID)
	}
	joined := ""
	for _, r := range out[0].Reasons {
		joined += r + " "
	}
	if !containsAll(joined, "same_service", "trust:T3") {
		t.Fatalf("reasons = %v", out[0].Reasons)
	}
}

func TestFuseDedupPerDoc(t *testing.T) {
	now := time.Now()
	// 同一文档 4 个 chunk 全部高分 → 最多保留 2 个
	lanes := map[string][]Evidence{
		"keyword": {
			mkEv("c1", "d1", SourceIncident, "order", TrustT3, now),
			mkEv("c2", "d1", SourceIncident, "order", TrustT3, now),
			mkEv("c3", "d1", SourceIncident, "order", TrustT3, now),
			mkEv("c4", "d1", SourceIncident, "order", TrustT3, now),
			mkEv("c5", "d2", SourceIncident, "pay", TrustT1, now),
		},
	}
	out := fuse(lanes, RetrievalQuery{Service: "order"}, 6, 2)
	perDoc := map[string]int{}
	for _, e := range out {
		perDoc[e.DocumentID]++
	}
	if perDoc["d1"] > 2 {
		t.Fatalf("doc d1 chunks = %d, want ≤2", perDoc["d1"])
	}
}

func TestFuseDiversity(t *testing.T) {
	now := time.Now()
	lanes := map[string][]Evidence{
		"keyword": {
			mkEv("i1", "i1", SourceIncident, "order", TrustT3, now),
			mkEv("i2", "i2", SourceIncident, "order", TrustT1, now),
			// 剧本分低于两条事故
			{ChunkID: "p1", DocumentID: "p1", SourceType: SourcePlaybook, SourceID: "p1",
				Title: "剧本：rollback", Content: "x", TrustLevel: TrustT2, Time: now, Reasons: []string{"keyword"}},
		},
	}
	out := fuse(lanes, RetrievalQuery{Service: "order"}, 6, 2)
	hasPlaybook := false
	for _, e := range out {
		if e.SourceType == SourcePlaybook {
			hasPlaybook = true
		}
	}
	if !hasPlaybook {
		t.Fatalf("diversity swap failed: %+v", out)
	}
}

func TestFuseTopK(t *testing.T) {
	now := time.Now()
	lanes := map[string][]Evidence{"keyword": {}}
	for i := 0; i < 10; i++ {
		lanes["keyword"] = append(lanes["keyword"], mkEv(
			"c"+string(rune('a'+i)), "d"+string(rune('a'+i)), SourceIncident, "", TrustT1, now))
	}
	out := fuse(lanes, RetrievalQuery{}, 4, 2)
	if len(out) != 4 {
		t.Fatalf("topK = %d, want 4", len(out))
	}
}

func TestSelectWithBudget(t *testing.T) {
	now := time.Now()
	long := Evidence{DocumentID: "d1", ChunkID: "c1", TrustLevel: TrustT3, Time: now,
		Content: "现象：连接池耗尽 " + repeatStr("字", 3000)}
	short := Evidence{DocumentID: "d2", ChunkID: "c2", TrustLevel: TrustT1, Time: now, Content: "x"}
	// 单文档 40% 上限：长内容应被 40% 规则拦下，短内容入选
	out := selectWithBudget([]Evidence{long, short}, 1000)
	if len(out) != 1 || out[0].ChunkID != "c2" {
		t.Fatalf("budget selection = %+v", out)
	}
	// 无预算限制 → 原样
	if out2 := selectWithBudget([]Evidence{long}, 0); len(out2) != 1 {
		t.Fatal("no budget must pass through")
	}
}

func TestTruncateStrKeepsUTF8(t *testing.T) {
	in := "错误：连接池耗尽" + strings.Repeat("池", 2000)
	out := truncateStr(in, 10)
	for _, r := range out {
		if r == '�' {
			t.Fatal("truncate produced invalid UTF-8")
		}
	}
}

func TestRecallVectorSkippedWithoutEmbedding(t *testing.T) {
	s := NewService(nil, Options{})
	_, err := s.recallVector(t.Context(), RetrievalQuery{TenantID: "t"})
	var skipped *laneSkipped
	if !errors.As(err, &skipped) || skipped.reason != DegradedVectorDisabled {
		t.Fatalf("err = %v, want vector_disabled skip", err)
	}
}

func TestChunkerSanityLongBody(t *testing.T) {
	// 超长事故正文 → 多 chunk，且每 chunk 带来源头
	var sb []string
	for i := 0; i < 200; i++ {
		sb = append(sb, "现象：告警 "+repeatStr("警", 60)+" 轮次"+string(rune('0'+i%10)))
	}
	chunks := ChunkBody(joinLines(sb), chunkSource{Title: "t", Service: "s", Version: 3})
	if len(chunks) < 2 {
		t.Fatalf("long body chunks = %d", len(chunks))
	}
	for _, c := range chunks {
		if !containsAll(c.Content, "版本：v3") {
			t.Fatalf("chunk missing provenance:\n%s", c.Content)
		}
	}
}

func repeatStr(s string, n int) string { return strings.Repeat(s, n) }

func joinLines(ss []string) string { return strings.Join(ss, "\n") }

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
