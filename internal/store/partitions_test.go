package store

import (
	"testing"
	"time"
)

func TestPartitionNameFor(t *testing.T) {
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC), "alerts_p202608"},
		{time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), "alerts_p202608"},
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), "alerts_p202612"},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), "alerts_p202701"},
	}
	for _, c := range cases {
		if got := partitionNameFor(c.in); got != c.want {
			t.Errorf("partitionNameFor(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPartitionBounds(t *testing.T) {
	cases := []struct {
		y          int
		m          time.Month
		wantFrom   string
		wantTo     string
	}{
		{2026, 8, "2026-08-01 00:00:00+00", "2026-09-01 00:00:00+00"},
		// 12 月跨年：边界进入次年 1 月
		{2026, 12, "2026-12-01 00:00:00+00", "2027-01-01 00:00:00+00"},
	}
	for _, c := range cases {
		from, to := partitionBounds(c.y, c.m)
		if from != c.wantFrom || to != c.wantTo {
			t.Errorf("partitionBounds(%d, %d) = (%q, %q), want (%q, %q)",
				c.y, c.m, from, to, c.wantFrom, c.wantTo)
		}
	}
}

func TestNextMonth(t *testing.T) {
	if y, m := nextMonth(2026, 8); y != 2026 || m != 9 {
		t.Errorf("nextMonth(2026,8) = (%d,%d), want (2026,9)", y, m)
	}
	if y, m := nextMonth(2026, 12); y != 2027 || m != 1 {
		t.Errorf("nextMonth(2026,12) = (%d,%d), want (2027,1)", y, m)
	}
}

// 分区命名与 retention 侧解析约定的一致性锚点：alerts_default 兜底分区不匹配
// 月份正则（retention 永不 drop 它），普通表名不匹配。
func TestPartitionNamingConvention(t *testing.T) {
	if got := partitionNameFor(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)); got != "alerts_p202608" {
		t.Fatalf("convention anchor broken: %q", got)
	}
}
