package util

import (
	"testing"
)

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID()
		if len(id) != 36 {
			t.Fatalf("NewID length = %d, want 36", len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestSHA256Deterministic(t *testing.T) {
	a := SHA256("hello")
	b := SHA256("hello")
	if a != b {
		t.Fatalf("SHA256 not deterministic: %s != %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("SHA256 length = %d, want 64", len(a))
	}
}

func TestHashEventStable(t *testing.T) {
	// 相同内容 → 相同哈希（与 map 插入顺序无关，json.Marshal 规范化 key）
	a := HashEvent(map[string]any{"a": 1, "b": 2})
	c := HashEvent(map[string]any{"b": 2, "a": 1})
	if a != c {
		t.Fatal("same content should hash identically regardless of map order")
	}
	// 内容不同 → 哈希不同
	d := HashEvent(map[string]any{"a": 1, "b": 3})
	if a == d {
		t.Fatal("different content should hash differently")
	}
}
