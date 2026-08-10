package rag

import (
	"strings"
	"testing"
	"time"
)

func TestParseFrontMatter(t *testing.T) {
	// 标准 front matter
	content := "---\ntitle: MySQL 连接池耗尽\nservice: pay\nupdated_at: 2026-08-01\n---\n正文第一行\n正文第二行"
	fm, body := ParseFrontMatter(content)
	if fm.Title != "MySQL 连接池耗尽" || fm.Service != "pay" {
		t.Fatalf("fm wrong: %+v", fm)
	}
	if fm.UpdatedAt == nil || !fm.UpdatedAt.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("updated_at wrong: %+v", fm.UpdatedAt)
	}
	if body != "正文第一行\n正文第二行" {
		t.Fatalf("body wrong: %q", body)
	}
	// tags 多种格式
	for _, tags := range []string{"[a, b]", "\"a,b\"", "a,b", "a b"} {
		fm2, _ := ParseFrontMatter("---\ntags: " + tags + "\n---\nbody")
		if len(fm2.Tags) != 2 || fm2.Tags[0] != "a" || fm2.Tags[1] != "b" {
			t.Fatalf("tags %q → %v", tags, fm2.Tags)
		}
	}
	// 非白名单字段忽略（含来源行不报错）
	fm3, _ := ParseFrontMatter("---\ntitle: t\nsource: 内部手册\n---\nbody")
	if fm3.Title != "t" || fm3.Service != "" {
		t.Fatalf("whitelist wrong: %+v", fm3)
	}
	// 无 front matter → body=全文
	plain := "# 标题\n正文"
	fm4, body4 := ParseFrontMatter(plain)
	if fm4.Title != "" || body4 != plain {
		t.Fatalf("no-fm wrong: %+v %q", fm4, body4)
	}
	// 正文中的 --- 不误伤（水平线）
	hr := "---\ntitle: t\n---\n前文\n\n---\n\n后文"
	_, body5 := ParseFrontMatter(hr)
	if body5 != "前文\n\n---\n\n后文" {
		t.Fatalf("hr in body must survive: %q", body5)
	}
	// BOM 容忍
	fm6, body6 := ParseFrontMatter("\uFEFF---\ntitle: t\n---\nbody")
	if fm6.Title != "t" || body6 != "body" {
		t.Fatalf("BOM handling wrong: %+v %q", fm6, body6)
	}
}

func TestBuildRunbook(t *testing.T) {
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// published → T2/verified/Index
	d := BuildRunbook(RunbookInfo{
		ID: "r1", TenantID: "t", Name: "连接池排障", Service: "pay",
		Tags: []string{"mysql", "pool"}, Content: "1. 查看连接数\n2. 重启池",
		Status: SrcPublished, UpdatedAt: at, Version: 3,
	})
	if !d.Index || d.Trust != TrustT2 || d.Verif != VerifVerified {
		t.Fatalf("published runbook wrong: %+v", d)
	}
	if d.Source.SourceType != SourceRunbook || d.Source.SourceID != "r1" || d.Source.SourceVersion != 3 {
		t.Fatalf("source key wrong: %+v", d.Source)
	}
	if d.Title != "runbook：连接池排障" {
		t.Fatalf("title prefix wrong: %q", d.Title)
	}
	if !strings.Contains(d.Body, "标题：连接池排障") || !strings.Contains(d.Body, "服务：pay") ||
		!strings.Contains(d.Body, "标签：mysql, pool") || !strings.Contains(d.Body, "1. 查看连接数") ||
		!strings.Contains(d.Body, "更新时间：") {
		t.Fatalf("body format wrong:\n%s", d.Body)
	}
	if tags, ok := d.Meta["tags"].([]string); !ok || len(tags) != 2 {
		t.Fatalf("meta tags wrong: %+v", d.Meta)
	}
	// draft → 不建文档
	d2 := BuildRunbook(RunbookInfo{ID: "r2", TenantID: "t", Name: "x", Status: SrcDraft, UpdatedAt: at})
	if d2.Index {
		t.Fatal("draft must not index")
	}
}

func TestEvalHelpers(t *testing.T) {
	// docHits 去重 + 只算期望集合
	evs := []Evidence{
		{DocumentID: "a"}, {DocumentID: "b"}, {DocumentID: "a"},
	}
	got := docHits(evs, toSet([]string{"a", "c"}))
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("docHits wrong: %v", got)
	}
	// p95
	if p95([]int64{}) != 0 {
		t.Fatal("p95 empty must be 0")
	}
	if p95([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) != 10 {
		t.Fatalf("p95 wrong: %v", p95([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}))
	}
}
