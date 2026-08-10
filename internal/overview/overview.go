// Package overview 实现总览聚合（KEEPON 5.1）：一次查询回答"现在有没有事、哪里有事"。
// 数据全部来自数据库重查（页面刷新后可恢复，不依赖 WS 内存）；alerts 分区表聚合
// 必须带 created_at 条件触发分区裁剪。
package overview

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"aiops/internal/backup"

	"gorm.io/gorm"
)

// Counts 关键计数。
type Counts struct {
	ActiveAlerts    int64 `json:"activeAlerts"`    // 活跃告警（last_alerts 非 resolved）
	ActiveIncidents int64 `json:"activeIncidents"` // 活跃事故（open/pending_approval）
	RunningTasks    int64 `json:"runningTasks"`    // 队列深度
	KnowledgeDocs   int64 `json:"knowledgeDocs"`   // 知识库条目（active）
}

// TrendPoint 告警趋势（小时桶，Go 侧补零）。
type TrendPoint struct {
	Hour  time.Time `json:"hour"`
	Count int64     `json:"count"`
}

// trendRow 趋势查询结果行。
type trendRow struct {
	Hour  time.Time
	Count int64
}

// EventItem 最近事件（alert=告警变化 / audit=处置动作；由 DB 重查）。
type EventItem struct {
	Kind      string    `json:"kind"` // alert | audit
	Title     string    `json:"title"`
	Severity  string    `json:"severity,omitempty"`
	Status    string    `json:"status,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Action    string    `json:"action,omitempty"`
	Actor     string    `json:"actor,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// BackupInfo 最近备份摘要。
type BackupInfo struct {
	Status       string    `json:"status"`
	VerifyStatus string    `json:"verifyStatus"`
	StartedAt    time.Time `json:"startedAt"`
	FileSize     int64     `json:"fileSize"`
}

// SystemHealth 系统健康。
type SystemHealth struct {
	DBOk       bool    `json:"dbOk"`
	DBError    string  `json:"dbError,omitempty"`
	QueueDepth int     `json:"queueDepth"`
	RAGBacklog int     `json:"ragBacklog"`
	RAGLagSec  float64 `json:"ragLagSec"`
	UptimeSec  int64   `json:"uptimeSec"`
}

// Overview 总览聚合 DTO。
type Overview struct {
	Window        string           `json:"window"`
	Counts        Counts           `json:"counts"`
	Trend         []TrendPoint     `json:"trend"`
	SeverityDist  map[string]int64 `json:"severityDist"` // 固定 5 键 critical..info
	RecentEvents  []EventItem      `json:"recentEvents"`
	System        SystemHealth     `json:"system"`
	BackupEnabled bool             `json:"backupEnabled"`
	LatestBackup  *BackupInfo      `json:"latestBackup,omitempty"`
}

// queueLener 队列深度（与 metrics 注册同模式）。
type queueLener interface{ Len() int }

// RAGStatuser RAG 积压/滞后（装配层注入；RAG 关闭时传 nil 接口——typed-nil 由装配层显式转换）。
type RAGStatuser interface {
	Backlog() int
	LagSeconds() float64
}

// BackupLister 最近成功备份（BackupEnabled=false 时传 nil 接口）。
type BackupLister interface {
	LastSuccess(ctx context.Context) (*backup.BackupRun, error)
}

// Service 总览聚合服务。
type Service struct {
	db        *gorm.DB
	sqlDB     *sql.DB
	q         queueLener
	rag       RAGStatuser
	bk        BackupLister
	startTime time.Time
}

// New 创建总览服务（rag/bk 传 nil：对应字段零值展示）。
func New(db *gorm.DB, sqlDB *sql.DB, q queueLener, rag RAGStatuser, bk BackupLister, startTime time.Time) *Service {
	return &Service{db: db, sqlDB: sqlDB, q: q, rag: rag, bk: bk, startTime: startTime}
}

// Get 聚合总览（window 限制 <= 7d 由 handler 校验）。
func (s *Service) Get(ctx context.Context, tenantID string, window time.Duration) (*Overview, error) {
	now := time.Now().UTC()
	since := now.Add(-window)
	ov := &Overview{
		Window:       window.String(),
		SeverityDist: map[string]int64{"critical": 0, "high": 0, "medium": 0, "low": 0, "info": 0},
	}

	// 计数（4 条独立小查询；单人规模无并发聚合必要）
	if err := s.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM last_alerts WHERE tenant_id = ? AND status <> 'resolved'`, tenantID,
	).Scan(&ov.Counts.ActiveAlerts).Error; err != nil {
		return nil, fmt.Errorf("overview: active alerts: %w", err)
	}
	if err := s.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM incidents WHERE tenant_id = ? AND status NOT IN ('resolved','merged')`, tenantID,
	).Scan(&ov.Counts.ActiveIncidents).Error; err != nil {
		return nil, fmt.Errorf("overview: active incidents: %w", err)
	}
	if err := s.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM knowledge_documents WHERE tenant_id = ? AND status = 'active'`, tenantID,
	).Scan(&ov.Counts.KnowledgeDocs).Error; err != nil {
		slog.Warn("overview: knowledge docs count", "err", err) // 表存在但异常时降级
	}
	if s.q != nil {
		ov.Counts.RunningTasks = int64(s.q.Len())
	}

	// 趋势（分区表：created_at 条件触发分区裁剪）
	var rows []trendRow
	if err := s.db.WithContext(ctx).Raw(
		`SELECT date_trunc('hour', created_at) AS hour, count(*) AS count
		 FROM alerts WHERE tenant_id = ? AND created_at >= ?
		 GROUP BY 1`, tenantID, since,
	).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("overview: trend: %w", err)
	}
	ov.Trend = fillTrend(rows, since, now)

	// 严重度分布
	type distRow struct {
		Severity string
		Count    int64
	}
	var dist []distRow
	if err := s.db.WithContext(ctx).Raw(
		`SELECT severity, count(*) AS count FROM alerts
		 WHERE tenant_id = ? AND created_at >= ? GROUP BY severity`, tenantID, since,
	).Scan(&dist).Error; err != nil {
		return nil, fmt.Errorf("overview: severity dist: %w", err)
	}
	for _, d := range dist {
		ov.SeverityDist[d.Severity] = d.Count
	}

	// 最近事件：告警 + 审计两路各 10 条，Go 归并排序取 20（字段形状不同不 UNION）
	ov.RecentEvents = s.recentEvents(ctx, tenantID, since)

	// 系统健康
	ov.System = s.systemHealth(ctx)

	// 备份
	if s.bk != nil {
		ov.BackupEnabled = true
		if last, err := s.bk.LastSuccess(ctx); err == nil && last != nil {
			ov.LatestBackup = &BackupInfo{
				Status: last.Status, VerifyStatus: last.VerifyStatus,
				StartedAt: last.StartedAt, FileSize: last.FileSize,
			}
		}
	}
	return ov, nil
}

// fillTrend 按小时补零（date_trunc 起点到当前整点逐桶）。
func fillTrend(rows []trendRow, since, now time.Time) []TrendPoint {
	bucket := func(t time.Time) time.Time { return t.Truncate(time.Hour) }
	start, end := bucket(since), bucket(now)
	byHour := make(map[time.Time]int64, len(rows))
	for _, r := range rows {
		byHour[r.Hour.UTC()] = r.Count
	}
	out := make([]TrendPoint, 0, int(end.Sub(start)/time.Hour)+1)
	for h := start; !h.After(end); h = h.Add(time.Hour) {
		out = append(out, TrendPoint{Hour: h, Count: byHour[h]})
	}
	return out
}

// recentEvents 告警 + 审计两路合并（按时间倒序，各限 10 条）。
func (s *Service) recentEvents(ctx context.Context, tenantID string, since time.Time) []EventItem {
	var alerts []EventItem
	if err := s.db.WithContext(ctx).Raw(
		`SELECT name AS title, severity, status, provider, created_at FROM alerts
		 WHERE tenant_id = ? AND created_at >= ?
		 ORDER BY created_at DESC LIMIT 10`, tenantID, since,
	).Scan(&alerts).Error; err != nil {
		slog.Warn("overview: recent alerts", "err", err)
	}
	for i := range alerts {
		alerts[i].Kind = "alert"
	}
	var audits []EventItem
	if err := s.db.WithContext(ctx).Raw(
		`SELECT action, actor, reason AS title, created_at FROM audit_logs
		 WHERE tenant_id = ? AND created_at >= ?
		 ORDER BY created_at DESC LIMIT 10`, tenantID, since,
	).Scan(&audits).Error; err != nil {
		slog.Warn("overview: recent audits", "err", err)
	}
	for i := range audits {
		audits[i].Kind = "audit"
	}
	merged := append(alerts, audits...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].CreatedAt.After(merged[j].CreatedAt) })
	if len(merged) > 20 {
		merged = merged[:20]
	}
	return merged
}

// systemHealth DB ping + 队列 + RAG + uptime。
func (s *Service) systemHealth(ctx context.Context) SystemHealth {
	h := SystemHealth{UptimeSec: int64(time.Since(s.startTime).Seconds())}
	if s.sqlDB != nil {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := s.sqlDB.PingContext(pingCtx); err != nil {
			h.DBOk = false
			h.DBError = truncate(err.Error(), 200)
		} else {
			h.DBOk = true
		}
		cancel()
	}
	if s.q != nil {
		h.QueueDepth = s.q.Len()
	}
	if s.rag != nil {
		h.RAGBacklog = s.rag.Backlog()
		h.RAGLagSec = s.rag.LagSeconds()
	}
	return h
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
