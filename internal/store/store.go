// Package store 负责 GORM 初始化。存储统一使用 PostgreSQL（docker-compose 提供），
// 其 JSONB 类型对告警事件快照的检索友好。SQLite 不作为备选（demo 级方案）。
package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"aiops/internal/alert"
	"aiops/internal/auth"
	"aiops/internal/eventbus"
	"aiops/internal/incident"
	"aiops/internal/maintenance"
	"aiops/internal/notifier"
	"aiops/internal/playbook"
	"aiops/internal/provider"
	"aiops/internal/topology"
	"aiops/internal/workflow"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// slogWriter 将 GORM 日志桥接到 slog（gorm 要求实现 Printf）。
type slogWriter struct{ l *slog.Logger }

func (w slogWriter) Printf(format string, args ...any) {
	w.l.Info("gorm: "+format, "args", args)
}

// DefaultDSN docker-compose 提供的 aiops PostgreSQL。
const DefaultDSN = "host=127.0.0.1 user=aiops password=aiops dbname=aiops port=5432 sslmode=disable TimeZone=Asia/Shanghai"

// Options 连接池与启动重试参数（Phase 3 存储加固）。
// 默认值对齐生产常见取值；零值字段自动回退默认，与 config 默认一致。
type Options struct {
	MaxOpenConns    int           // 连接池最大打开连接数（默认 20）
	MaxIdleConns    int           // 最大空闲连接数（默认 = MaxOpen，防连接抖动；Windows 回环 TIME_WAIT 冲突会引发 dial 错误）
	ConnMaxLifetime time.Duration // 连接最长存活（默认 30min；<=0 表示不限）
	ConnMaxIdleTime time.Duration // 空闲连接回收（默认 5min；<=0 表示不限）
	ConnectRetries  int           // 启动连接重试次数（默认 5，postgres 未就绪时等待拉起）
}

// Open 打开 PostgreSQL 连接：连接池调优 + 启动重试 + 自动迁移。
func Open(dsn string, opts Options) (*gorm.DB, error) {
	if dsn == "" {
		dsn = DefaultDSN
	}
	applyDefaults(&opts)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.New(slogWriter{slog.Default()}, logger.Config{
			SlowThreshold: time.Second,
			LogLevel:      logger.Warn,
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("store: open postgres: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("store: sql db: %w", err)
	}
	// 连接池调优：压测/告警洪峰时避免连接耗尽与长连接老化
	sqlDB.SetMaxOpenConns(opts.MaxOpenConns)
	sqlDB.SetMaxIdleConns(opts.MaxIdleConns)
	if opts.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(opts.ConnMaxLifetime)
	}
	if opts.ConnMaxIdleTime > 0 {
		sqlDB.SetConnMaxIdleTime(opts.ConnMaxIdleTime)
	}
	// 启动重试：docker-compose 的 postgres 可能仍在健康检查窗口内
	if err := pingWithRetry(sqlDB, opts.ConnectRetries); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, err
	}
	return db, nil
}

// applyDefaults 零值回退默认（与 config 包默认值保持一致，方便直接调用的测试）。
func applyDefaults(o *Options) {
	if o.MaxOpenConns <= 0 {
		o.MaxOpenConns = 20
	}
	if o.MaxIdleConns <= 0 {
		o.MaxIdleConns = o.MaxOpenConns // 与 MaxOpen 对齐：空闲连接不回收重连，避免回环 TIME_WAIT 抖动
	}
	if o.ConnMaxLifetime <= 0 {
		o.ConnMaxLifetime = 30 * time.Minute
	}
	if o.ConnMaxIdleTime <= 0 {
		o.ConnMaxIdleTime = 5 * time.Minute
	}
	if o.ConnectRetries <= 0 {
		o.ConnectRetries = 5
	}
}

// pingWithRetry 带退避重试的连通性检查（重试次数含首次，间隔 2s）。
func pingWithRetry(sqlDB interface{ PingContext(context.Context) error }, retries int) error {
	var lastErr error
	for i := 0; i < retries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		lastErr = sqlDB.PingContext(ctx)
		cancel()
		if lastErr == nil {
			return nil
		}
		slog.Warn("store: postgres not ready, retrying", "attempt", i+1, "max", retries, "err", lastErr)
		if i < retries-1 {
			time.Sleep(2 * time.Second)
		}
	}
	return fmt.Errorf("store: postgres unreachable after %d attempts: %w", retries, lastErr)
}

// migrate 自动建表。所有模型在这里集中注册。
// AutoMigrate 会补齐模型标签中新增的索引（如保留策略清理用的 created_at 索引）。
func migrate(db *gorm.DB) error {
	models := []any{
		&alert.Alert{},
		&alert.LastAlert{},
		&incident.Incident{},
		&incident.AlertToIncident{},
		&incident.State{},
		&incident.Rule{},
		&provider.ProviderConfig{},
		&topology.TopologyService{},
		&topology.Edge{},
		&playbook.Playbook{},
		&maintenance.Window{},
		&notifier.ChannelConfig{},
		&workflow.Workflow{},
		&workflow.WorkflowRun{},
		&auth.APIKey{},
		&eventbus.AuditLog{},
	}
	return db.AutoMigrate(models...)
}
