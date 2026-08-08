package retention

import (
	"regexp"
	"strconv"
	"time"
)

// 月分区名白名单：alerts_pYYYYMM（与 store 分区命名约定一致）。
// 仅匹配该格式的分区才允许 DROP；alerts_default 兜底分区等不匹配 → 永不 drop。
var monthPartitionRe = regexp.MustCompile(`^alerts_p(\d{6})$`)

// parsePartitionMonth 解析月分区名（如 alerts_p202608 → 2026, 8, true）。
// 非月分区名（alerts_default/alerts_legacy 等）返回 ok=false。
func parsePartitionMonth(name string) (year int, month time.Month, ok bool) {
	m := monthPartitionRe.FindStringSubmatch(name)
	if m == nil {
		return 0, 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, 0, false
	}
	// 格式 YYYYMM：前 4 位年份，后 2 位月份
	y, mo := n/100, n%100
	if mo < 1 || mo > 12 {
		return 0, 0, false
	}
	return y, time.Month(mo), true
}

// partitionEnd 返回 (year, month) 分区的边界结束时刻：下月 1 日 00:00:00 UTC
// （分区左闭右开 [月初, 下月 1 日)，此即整个分区数据的最大时刻上限）。
func partitionEnd(y int, m time.Month) time.Time {
	ny, nm := y, m+1
	if m == 12 {
		ny, nm = y+1, 1
	}
	return time.Date(ny, nm, 1, 0, 0, 0, 0, time.UTC)
}
