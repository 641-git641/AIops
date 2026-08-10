package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// rotate.go：备份轮转（数量 + 总容量双限，任一达限删最旧）。
// 只删本服务命名的 aiops_*.dump / aiops_*.dump.aes，绝不触碰其他文件。

// backupGlob 备份文件命名（mirror 加密产物追加 .aes）。
const backupGlob = "aiops_*.dump*"

// Rotate 清理 dir 下超出上限（keep 份数 / maxBytes 总容量）的备份文件，返回被删文件。
func Rotate(dir string, keep int, maxBytes int64) (removed []string, err error) {
	entries, err := filepath.Glob(filepath.Join(dir, backupGlob))
	if err != nil {
		return nil, err
	}
	type f struct {
		path string
		size int64
	}
	files := make([]f, 0, len(entries))
	for _, p := range entries {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		files = append(files, f{p, info.Size()})
	}
	// 按名称（时间戳在前）排序 = 按时间从旧到新
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	var total int64
	for _, x := range files {
		total += x.size
	}
	for _, x := range files {
		overCount := len(files) > keep
		overBytes := total > maxBytes
		if !overCount && !overBytes {
			break
		}
		if err := os.Remove(x.path); err != nil {
			return removed, fmt.Errorf("backup: 轮转删除 %s: %w", x.path, err)
		}
		removed = append(removed, x.path)
		total -= x.size
		files = files[1:]
	}
	return removed, nil
}

// IsBackupFile 判断文件名是否为备份产物（供清理/删除保护用）。
func IsBackupFile(name string) bool {
	return strings.HasPrefix(name, "aiops_") && strings.Contains(name, ".dump")
}
