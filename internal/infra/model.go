// Package infra 采集基础设施运行指标（监控平板/总览的数据源）：
// 主机 CPU/内存/进程、PostgreSQL 运行状态、docker 容器资源占用。
// 全部实时采集（端点被调用时采样 + 环形缓冲保留近期历史供趋势），无持久化存储。
package infra

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"
)

// HostStats 主机资源。
type HostStats struct {
	CPUPercent        float64 `json:"cpuPercent"` // 系统 CPU 使用率（两次采样差）
	MemTotalMB        float64 `json:"memTotalMb"`
	MemUsedMB         float64 `json:"memUsedMb"`
	MemPercent        float64 `json:"memPercent"`
	ProcessCPUPercent float64 `json:"processCpuPercent"` // 本进程 CPU 使用率
	GoHeapMB          float64 `json:"goHeapMb"`
	GoRoutines        int     `json:"goRoutines"`
	DiskFreeGB        float64 `json:"diskFreeGb"` // 工作目录所在盘剩余（Windows 有；失败 0）
	UptimeSec         int64   `json:"uptimeSec"`  // 进程运行时长
}

// PGStats PostgreSQL 运行状态（pg_stat_database 单库视角）。
type PGStats struct {
	Connections   int     `json:"connections"`   // 当前连接数（含空闲）
	ActiveQueries int     `json:"activeQueries"` // 活跃查询数
	XactCommit    int64   `json:"xactCommit"`    // 事务提交累计
	XactRollback  int64   `json:"xactRollback"`  // 事务回滚累计
	Deadlocks     int64   `json:"deadlocks"`     // 死锁累计
	CacheHitRatio float64 `json:"cacheHitRatio"` // 缓存命中率 %（0-100）
	DBSizeMB      float64 `json:"dbSizeMb"`      // 当前库大小
	BackendMax    int     `json:"backendMax"`    // 最大连接数
}

// ContainerStats docker 容器资源占用。
type ContainerStats struct {
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpuPercent"`
	MemPercent float64 `json:"memPercent"`
	MemUsedMB  float64 `json:"memUsedMb"`
	PIDs       int     `json:"pids"`
}

// HistoryPoint 采样历史点（环形缓冲，趋势图用）。
type HistoryPoint struct {
	SampledAt     time.Time `json:"sampledAt"`
	CPUPercent    float64   `json:"cpuPercent"`
	MemPercent    float64   `json:"memPercent"`
	PGConnections int       `json:"pgConnections"`
}

// InfraSnapshot 单次基础设施快照。
type InfraSnapshot struct {
	Host       HostStats        `json:"host"`
	PG         PGStats          `json:"pg"`
	Containers []ContainerStats `json:"containers"`
	SampledAt  time.Time        `json:"sampledAt"`
	History    []HistoryPoint   `json:"history"` // 近 N 点（趋势）
}

// Service 基础设施指标服务。
type Service struct {
	db       *gorm.DB
	mu       sync.Mutex
	hist     []HistoryPoint // 环形缓冲
	maxHis   int
	lastCPU  cpuSample // 上次 CPU 采样（计算使用率）
	lastProc cpuSample
	start    time.Time
}

// New 创建服务（histPoints 默认 60 点）。
func New(db *gorm.DB) *Service {
	return &Service{db: db, maxHis: 60, start: time.Now()}
}

// Get 采集一次快照并追加历史（幂等廉价；docker 不可用时容器列表为空）。
func (s *Service) Get(ctx context.Context) InfraSnapshot {
	snap := InfraSnapshot{SampledAt: time.Now().UTC()}
	snap.Host = collectHost(&s.lastCPU, &s.lastProc, s.start)
	if s.db != nil {
		snap.PG = collectPG(ctx, s.db)
	}
	snap.Containers = collectDocker(ctx)

	s.mu.Lock()
	s.hist = append(s.hist, HistoryPoint{
		SampledAt: snap.SampledAt, CPUPercent: snap.Host.CPUPercent,
		MemPercent: snap.Host.MemPercent, PGConnections: snap.PG.Connections,
	})
	if len(s.hist) > s.maxHis {
		s.hist = s.hist[len(s.hist)-s.maxHis:]
	}
	snap.History = append([]HistoryPoint(nil), s.hist...)
	s.mu.Unlock()
	return snap
}

// cpuSample CPU 计数器快照。
type cpuSample struct {
	idle, kernel, user uint64 // 系统级
	pKernel, pUser     uint64 // 进程级
	wall               time.Time
	pWall              time.Time // 进程级采样时刻
}

// collectPG PostgreSQL 运行状态。
func collectPG(ctx context.Context, db *gorm.DB) PGStats {
	var st PGStats
	row := db.WithContext(ctx).Raw(`
		SELECT
			(SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()) AS connections,
			(SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND state = 'active') AS active_queries,
			(SELECT xact_commit FROM pg_stat_database WHERE datname = current_database()) AS xact_commit,
			(SELECT xact_rollback FROM pg_stat_database WHERE datname = current_database()) AS xact_rollback,
			(SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()) AS deadlocks,
			(SELECT blks_hit FROM pg_stat_database WHERE datname = current_database()) AS blks_hit,
			(SELECT blks_read FROM pg_stat_database WHERE datname = current_database()) AS blks_read,
			(SELECT pg_database_size(current_database())::float8 / 1048576) AS db_size_mb,
			(SELECT setting::int FROM pg_settings WHERE name = 'max_connections') AS backend_max`).
		Row()
	var blksHit, blksRead int64
	err := row.Scan(&st.Connections, &st.ActiveQueries, &st.XactCommit, &st.XactRollback,
		&st.Deadlocks, &blksHit, &blksRead, &st.DBSizeMB, &st.BackendMax)
	if err != nil {
		slog.Warn("infra: pg stats", "err", err)
		return st
	}
	total := blksHit + blksRead
	if total > 0 {
		st.CacheHitRatio = float64(blksHit) / float64(total) * 100
	}
	return st
}
