// Package retention 实现数据保留策略（Phase 3 存储加固）：
// 告警明细/审计/工作流运行记录按天数分级保留，worker 定时分批删除，
// 防止数据无限膨胀拖垮查询与备份。事故与 last_alerts（当前状态）不参与清理。
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
)

// Rule 一条保留规则：某表按时间列清理早于 cutoff 的行。
// 表名与时间列均为包内常量（非用户输入），无注入面。
type Rule struct {
	Table      string // 表名（如 alerts）
	TimeColumn string // 时间列（如 created_at / started_at）
	Days       int    // 保留天数；<=0 表示该表不清理
}

// Service 保留策略服务：RunOnce 按规则分批删除（每批 batch 行，避免长事务锁表）。
type Service struct {
	db    *gorm.DB
	rules []Rule
	batch int // 每批删除行数（默认 5000）
}

// New 创建保留策略服务（内部过滤 Days <= 0 的规则；不修改入参 slice）。
func New(db *gorm.DB, rules []Rule) *Service {
	active := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if r.Days > 0 {
			active = append(active, r)
		}
	}
	return &Service{db: db, rules: active, batch: 5000}
}

// RunOnce 执行一次全部规则的清理。单条规则失败不影响其余（best-effort）。
func (s *Service) RunOnce(ctx context.Context) error {
	if len(s.rules) == 0 {
		return nil
	}
	for _, r := range s.rules {
		if err := s.cleanTable(ctx, r); err != nil {
			slog.Warn("retention: clean table", "table", r.Table, "err", err)
		}
	}
	return nil
}

// cleanTable 按「子查询取 id + 主表删除」分批删除，避免单条 DELETE 全表扫描加锁。
// 循环直到一批删不满（即过期行已清完）。
func (s *Service) cleanTable(ctx context.Context, r Rule) error {
	cutoff := time.Now().UTC().Add(-time.Duration(r.Days) * 24 * time.Hour)
	// 表名/列名来自包内常量，不做参数绑定
	selectID := fmt.Sprintf(
		"SELECT id FROM %s WHERE %s < ? ORDER BY %s LIMIT ?", r.Table, r.TimeColumn, r.TimeColumn)
	del := fmt.Sprintf("DELETE FROM %s WHERE id IN (%s)", r.Table, selectID)
	total := 0
	for {
		// 每批一个短事务（GORM Exec 自带事务）；ctx 取消（如优雅退出）时中止
		res := s.db.WithContext(ctx).Exec(del, cutoff, s.batch)
		if res.Error != nil {
			return fmt.Errorf("retention: delete %s: %w", r.Table, res.Error)
		}
		affected := res.RowsAffected
		total += int(affected)
		if affected < int64(s.batch) {
			break
		}
	}
	if total > 0 {
		slog.Info("retention: cleaned", "table", r.Table, "days", r.Days, "rows", total)
	}
	return nil
}
