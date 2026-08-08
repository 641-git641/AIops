package retention

import (
	"testing"
	"time"
)

func TestParsePartitionMonth(t *testing.T) {
	cases := []struct {
		name  string
		y     int
		m     time.Month
		valid bool
	}{
		{"alerts_p202608", 2026, 8, true},
		{"alerts_p202612", 2026, 12, true},
		{"alerts_p202701", 2027, 1, true},
		// 非月分区名 / 非法月份 → 拒绝（永不 drop）
		{"alerts_default", 0, 0, false},
		{"alerts_legacy", 0, 0, false},
		{"alerts", 0, 0, false},
		{"alerts_p202613", 0, 0, false},
		{"alerts_p202600", 0, 0, false},
		{"alerts_p", 0, 0, false},
	}
	for _, c := range cases {
		y, m, ok := parsePartitionMonth(c.name)
		if ok != c.valid {
			t.Errorf("parsePartitionMonth(%q) valid = %v, want %v", c.name, ok, c.valid)
			continue
		}
		if ok && (y != c.y || m != c.m) {
			t.Errorf("parsePartitionMonth(%q) = (%d,%d), want (%d,%d)", c.name, y, m, c.y, c.m)
		}
	}
}

func TestPartitionEnd(t *testing.T) {
	if got, want := partitionEnd(2026, 8), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("partitionEnd(2026,8) = %v, want %v", got, want)
	}
	// 12 月跨年
	if got, want := partitionEnd(2026, 12), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("partitionEnd(2026,12) = %v, want %v", got, want)
	}
}

// 过期判定锚点：30d 保留、cutoff=2026-07-09T00:00Z 时——
// 6 月整月过期（月末 7/1 <= cutoff）→ drop；7 月分区月末 8/1 > cutoff → 保留（行级 DELETE 兜底）。
func TestDropDecision(t *testing.T) {
	cutoff := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	if partitionEnd(2026, 6).After(cutoff) {
		t.Error("June partition should be fully expired (drop)")
	}
	if !partitionEnd(2026, 7).After(cutoff) {
		t.Error("July partition should be retained (row-level DELETE fallback)")
	}
}
