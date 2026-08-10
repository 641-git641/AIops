// Package anomaly 基于历史数据的第一版异常检测（KEEPON 七，P3）。
// 首期只对「告警数量」做简单季节性基线：过去 N 小时逐小时计数 vs 历史同期（同小时）
// 均值 ± 2σ 区间；超上限标记 anomaly。**只提示，不自动创建事故/触发自愈/发通知**。
// 评估误报率后再逐步提高自动化程度。
package anomaly

import (
	"context"
	"math"
	"strconv"
	"time"

	"gorm.io/gorm"
)

// AlertAnomaly 单小时告警数与基线。
type AlertAnomaly struct {
	Hour     string  `json:"hour"`     // RFC3339（整点）
	Count    int     `json:"count"`    // 该小时告警数
	Baseline float64 `json:"baseline"` // 历史同期均值
	Lower    float64 `json:"lower"`    // 基线区间下界（≥0）
	Upper    float64 `json:"upper"`    // 基线区间上界
	Sigma    float64 `json:"sigma"`    // 偏移倍数（count 相对基线的 z 值；数据不足为 0）
	Anomaly  bool    `json:"anomaly"`  // count > upper 且历史样本充足
	Reason   string  `json:"reason,omitempty"`
}

// Service 异常检测服务。
type Service struct {
	db *gorm.DB
}

// NewService 创建服务。
func NewService(db *gorm.DB) *Service { return &Service{db: db} }

// Options 检测参数。
type Options struct {
	Hours       int // 观察窗口小时数（默认 24）
	HistoryDays int // 基线历史天数（默认 7；不足 3 个同小时样本标记样本不足）
	Sigma       float64 // 基线区间倍数（默认 2）
}

// AlertAnomalies 告警数量异常检测：一次 SQL 取窗口+历史全量，Go 侧按小时桶分组计算基线。
// 返回按时间升序的每小时结果（窗口内）；无历史数据时返回带 insufficient 标记的空基线。
func (s *Service) AlertAnomalies(ctx context.Context, tenantID string, opt Options) ([]AlertAnomaly, error) {
	if opt.Hours <= 0 {
		opt.Hours = 24
	}
	if opt.HistoryDays <= 0 {
		opt.HistoryDays = 7
	}
	if opt.Sigma <= 0 {
		opt.Sigma = 2
	}

	// 一次查询：窗口 + 历史（窗口在历史内，不重复取）
	var rows []struct {
		Hour  time.Time
		Count int
	}
	err := s.db.WithContext(ctx).Raw(`
		SELECT date_trunc('hour', created_at) AS hour, count(*) AS count
		FROM alerts
		WHERE tenant_id = ? AND created_at >= now() - (?::int || ' days')::interval
		GROUP BY 1 ORDER BY 1`,
		tenantID, opt.HistoryDays).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	// 组装窗口内每小时结果（基线样本**排除观察窗口内**的同小时数据——自己不比自己）
	now := time.Now().UTC().Truncate(time.Hour)
	windowStart := now.Add(-time.Duration(opt.Hours-1) * time.Hour)
	hist := make([][]int, 24)
	byHour := map[time.Time]int{}
	for _, r := range rows {
		h := r.Hour.UTC()
		if h.Before(windowStart) {
			hist[h.Hour()] = append(hist[h.Hour()], r.Count)
		}
		byHour[h] = r.Count
	}

	out := make([]AlertAnomaly, 0, opt.Hours)
	for i := opt.Hours - 1; i >= 0; i-- {
		h := now.Add(-time.Duration(i) * time.Hour)
		count := byHour[h]
		mean, std := meanStd(hist[h.Hour()])
		upper := mean + opt.Sigma*std
		lower := math.Max(0, mean-opt.Sigma*std)
		a := AlertAnomaly{
			Hour: h.Format(time.RFC3339), Count: count,
			Baseline: round1(mean), Lower: round1(lower), Upper: round1(upper),
		}
		if len(hist[h.Hour()]) < 3 {
			a.Reason = "历史样本不足（<3 天同小时数据）"
		} else if float64(count) > upper && upper > 0 {
			// 超出区间即异常（std=0 时区间收敛到均值，任何超量都算偏移）
			if std > 0 {
				a.Sigma = round1((float64(count) - mean) / std)
			} else {
				a.Sigma = round1(float64(count) - mean)
			}
			a.Anomaly = true
			a.Reason = "告警数超出基线上限（基线区间 " + fmtRange(lower, upper) + "）"
		}
		out = append(out, a)
	}
	return out, nil
}

// meanStd 均值与标准差（样本标准差；样本 <2 时 std=0）。
func meanStd(xs []int) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += float64(x)
	}
	mean := sum / float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	var ss float64
	for _, x := range xs {
		d := float64(x) - mean
		ss += d * d
	}
	return mean, math.Sqrt(ss / float64(len(xs)-1))
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// fmtRange 基线区间展示（整数时省略小数）。
func fmtRange(low, up float64) string {
	fmt1 := func(f float64) string {
		if f == math.Trunc(f) {
			return strconv.FormatFloat(f, 'f', 0, 64)
		}
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return "[" + fmt1(low) + ", " + fmt1(up) + "]"
}
