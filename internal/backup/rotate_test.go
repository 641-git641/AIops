package backup

import (
	"os"
	"path/filepath"
	"testing"
)

// touch 创建指定大小的备份假文件（时间戳按名称排序）。
func touch(t *testing.T, dir, name string, size int64) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
}

func TestRotateByCount(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		touch(t, dir, "aiops_20260809_00000"+string(rune('0'+i))+"_a.dump", 100)
	}
	removed, err := Rotate(dir, 3, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %d, want 2", len(removed))
	}
	left, _ := filepath.Glob(filepath.Join(dir, backupGlob))
	if len(left) != 3 {
		t.Fatalf("left = %d, want 3", len(left))
	}
}

func TestRotateByBytes(t *testing.T) {
	dir := t.TempDir()
	// 3 份 × 400B = 1.2KB，上限 1KB → 删最旧 1 份
	for i := 0; i < 3; i++ {
		touch(t, dir, "aiops_20260809_00000"+string(rune('0'+i))+"_a.dump", 400)
	}
	removed, err := Rotate(dir, 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || !IsBackupFile(filepath.Base(removed[0])) {
		t.Fatalf("removed = %v, want oldest single file", removed)
	}
}

func TestRotateLeavesForeignFiles(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "aiops_20260809_000000_a.dump", 10)
	touch(t, dir, "important.txt", 10<<20) // 非备份文件（10MB）
	removed, err := Rotate(dir, 0, 100)    // 双限都超 → 只应删备份文件
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %v, want only backup file", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "important.txt")); err != nil {
		t.Fatal("foreign file must not be touched")
	}
}

func TestRotateEncryptedMirror(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "aiops_20260809_000000_a.dump.aes", 10)
	removed, err := Rotate(dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("aes file not rotated: %v", removed)
	}
}
