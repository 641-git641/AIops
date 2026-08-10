package overview

import (
	"testing"
	"time"
)

func TestFillTrendZeros(t *testing.T) {
	base := time.Date(2026, 8, 10, 10, 30, 0, 0, time.UTC)
	since := base.Add(-3 * time.Hour)
	now := base
	// 只有 2 个小时有数据（含越界小时，应被忽略）
	rows := []trendRow{
		{Hour: since.Truncate(time.Hour), Count: 5},
		{Hour: since.Add(2 * time.Hour).Truncate(time.Hour), Count: 7},
		{Hour: since.Add(-2 * time.Hour).Truncate(time.Hour), Count: 99}, // 窗口外，不出现
	}
	trend := fillTrend(rows, since, now)
	if len(trend) != 4 { // 3h 窗口 + 当前整点 = 4 桶
		t.Fatalf("trend len = %d, want 4: %+v", len(trend), trend)
	}
	if trend[0].Count != 5 || trend[1].Count != 0 || trend[2].Count != 7 || trend[3].Count != 0 {
		t.Fatalf("fill wrong: %+v", trend)
	}
	// 时间单调
	for i := 1; i < len(trend); i++ {
		if !trend[i].Hour.After(trend[i-1].Hour) {
			t.Fatalf("trend not ascending at %d", i)
		}
	}
}
