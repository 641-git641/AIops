package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aiops/internal/metrics"
	"aiops/internal/util"

	"gorm.io/gorm"
)

// service.go：备份编排（KEEPON 4.1）。
// 流程：建 run 行 → pg_dump 流式写盘 + sha256 → 镜像（可选加密）→ 恢复校验 → 轮转 → 更新行。
// 手动触发与定时 worker 经 TryLock 串行；备份失败保留 ErrorDetail 与状态，不静默。

// ErrRunning 已有备份在执行（手动触发返回 409 语义）。
var ErrRunning = errors.New("backup already running")

// Config 备份服务配置（由 config.Config 映射）。
type Config struct {
	DSN            string        // 源库连接串（ParseDSN 解析）
	Dir            string        // 备份目录（0700 创建，文件 0600）
	Keep           int           // 本机保留份数（数量限）
	MaxBytes       int64         // 本机保留总容量上限（容量限）
	MirrorDir      string        // 异地镜像目录（空 = 不镜像）
	MirrorEncrypt  bool          // 镜像 AES-256-GCM 加密
	PassphraseFile string        // 加密口令文件（镜像加密必填）
	Verify         bool          // 备份后恢复临时库校验
	Timeout        time.Duration // 单次备份超时（CommandContext 兜底）
	Interval       time.Duration // 距上次成功超间隔才执行（补跑语义）
}

// Service 备份服务。
type Service struct {
	db      *gorm.DB
	cfg     Config
	ex      PgExecutor
	mu      sync.Mutex
	metrics *metrics.Registry // 未装配时 nil（Inc 内部安全跳过）
}

// New 创建备份服务。
func New(db *gorm.DB, cfg Config, ex PgExecutor) *Service {
	return &Service{db: db, cfg: cfg, ex: ex}
}

// SetMetrics 装配 Prometheus 指标（备份状态进 /metrics，总览可消费）。
func (s *Service) SetMetrics(reg *metrics.Registry) {
	s.metrics = reg
	reg.Gauge("aiops_backup_state", "上次备份状态（1=成功且未过期，0=失败/过期/无）", s.BackupState)
	reg.Gauge("aiops_backup_last_success_age_seconds", "距上次成功备份的秒数（无备份为 -1）", s.LastSuccessAge)
	reg.Counter("aiops_backup_runs_total", "备份执行次数（按结果）", "result")
}

// RunIfDue 距上次成功备份超过 interval 才执行（启动补跑 + 定时轮询入口）。
func (s *Service) RunIfDue(ctx context.Context) error {
	last, err := s.LastSuccess(ctx)
	if err == nil && last != nil && time.Since(*last.FinishedAt) < s.cfg.Interval {
		return nil
	}
	return s.RunOnce(ctx)
}

// RunOnce 执行一次定时备份（TryLock 串行，运行中返回 ErrRunning）。
func (s *Service) RunOnce(ctx context.Context) error {
	_, err := s.run(ctx, "schedule")
	return err
}

// RunManual 手动触发备份（HTTP 端点用；运行中返回 ErrRunning）。
func (s *Service) RunManual(ctx context.Context) (*BackupRun, error) {
	return s.run(ctx, "manual")
}

// run 串行入口。
func (s *Service) run(ctx context.Context, trigger string) (*BackupRun, error) {
	if !s.mu.TryLock() {
		return nil, ErrRunning
	}
	defer s.mu.Unlock()
	return s.runLocked(ctx, trigger)
}

// runLocked 执行完整备份流程（调用方持锁）。
func (s *Service) runLocked(ctx context.Context, trigger string) (*BackupRun, error) {
	now := time.Now().UTC()
	run := &BackupRun{ID: util.NewID(), Trigger: trigger, StartedAt: now, Status: "running"}
	if err := s.db.Create(run).Error; err != nil {
		return nil, err
	}
	fail := func(err error) (*BackupRun, error) {
		done := time.Now().UTC()
		run.Status = "failed"
		run.FinishedAt = &done
		run.ErrorDetail = truncateStr(err.Error(), 2048)
		if err2 := s.db.Save(run).Error; err2 != nil {
			slog.Error("backup: persist failure", "run", run.ID, "err", err2)
		}
		s.incMetric("aiops_backup_runs_total", "failed")
		slog.Error("backup failed", "run", run.ID, "trigger", trigger, "err", err)
		return run, err
	}

	target, err := ParseDSN(s.cfg.DSN)
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(s.cfg.Dir, 0o700); err != nil {
		return fail(fmt.Errorf("backup: 创建备份目录: %w", err))
	}

	// 1. pg_dump -Fc → 流式写盘 + sha256（毫秒时间戳 + run ID 前缀保证同秒多次备份不冲突）
	fname := fmt.Sprintf("aiops_%s_%s.dump", now.Format("20060102_150405.000"), run.ID[:8])
	path := filepath.Join(s.cfg.Dir, fname)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(fmt.Errorf("backup: 创建备份文件: %w", err))
	}
	hash := sha256.New()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	args := append([]string{"-Fc"}, target.pgArgs()...)
	args = append(args, "-d", target.DBName)
	stderr, err := s.ex.Exec(ctx, "pg_dump", args, nil, io.MultiWriter(file, hash))
	if cerr := file.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = err.Error()
		}
		return fail(fmt.Errorf("pg_dump: %s", msg))
	}
	info, _ := os.Stat(path)
	run.FilePath = path
	run.FileSize = info.Size()
	run.SHA256 = hex.EncodeToString(hash.Sum(nil))

	// 2. 镜像（同盘 rename / 跨盘 copy；可选 AES 加密）
	if s.cfg.MirrorDir != "" {
		if err := os.MkdirAll(s.cfg.MirrorDir, 0o700); err != nil {
			return fail(fmt.Errorf("backup: 创建镜像目录: %w", err))
		}
		mpath := filepath.Join(s.cfg.MirrorDir, fname)
		if s.cfg.MirrorEncrypt {
			if s.cfg.PassphraseFile == "" {
				return fail(fmt.Errorf("backup: 镜像加密已启用但未配置口令文件"))
			}
			pass, err := readPassphrase(s.cfg.PassphraseFile)
			if err != nil {
				return fail(err)
			}
			mpath += ".aes"
			if err := EncryptFile(path, mpath, pass); err != nil {
				return fail(fmt.Errorf("backup: 镜像加密: %w", err))
			}
			run.MirrorEncrypted = true
		} else if err := copyFile(path, mpath); err != nil {
			return fail(fmt.Errorf("backup: 镜像复制: %w", err))
		}
		run.MirrorPath = mpath
	}

	// 3. 恢复校验（临时库 + 关键表计数；校验失败独立呈现，备份仍算成功）
	if s.cfg.Verify {
		if err := s.VerifyFromFile(ctx, run); err != nil {
			run.VerifyStatus = "failed"
			run.VerifyDetail = truncateStr(err.Error(), 2048)
			slog.Warn("backup: verify failed", "run", run.ID, "err", err)
		} else {
			run.VerifyStatus = "passed"
		}
	} else {
		run.VerifyStatus = "skipped"
	}

	// 4. 轮转（本机 + 镜像同策略）
	if removed, err := Rotate(s.cfg.Dir, s.cfg.Keep, s.cfg.MaxBytes); err != nil {
		slog.Warn("backup: rotate local", "err", err)
	} else if len(removed) > 0 {
		slog.Info("backup: rotated", "removed", len(removed))
	}
	if s.cfg.MirrorDir != "" {
		if removed, err := Rotate(s.cfg.MirrorDir, s.cfg.Keep, s.cfg.MaxBytes); err != nil {
			slog.Warn("backup: rotate mirror", "err", err)
		} else if len(removed) > 0 {
			slog.Info("backup: rotated mirror", "removed", len(removed))
		}
	}

	done := time.Now().UTC()
	run.Status = "success"
	run.FinishedAt = &done
	if err := s.db.Save(run).Error; err != nil {
		return nil, err
	}
	s.incMetric("aiops_backup_runs_total", "success")
	slog.Info("backup: dump ok", "run", run.ID, "trigger", trigger, "size", run.FileSize,
		"sha256", run.SHA256[:12], "verify", run.VerifyStatus, "mirror", run.MirrorPath != "")
	return run, nil
}

// LastRun 最近一次执行记录。
func (s *Service) LastRun(ctx context.Context) (*BackupRun, error) {
	var run BackupRun
	err := s.db.WithContext(ctx).Order("started_at DESC").First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &run, err
}

// LastSuccess 最近一次成功备份。
func (s *Service) LastSuccess(ctx context.Context) (*BackupRun, error) {
	var run BackupRun
	err := s.db.WithContext(ctx).Where("status = ?", "success").Order("finished_at DESC").First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &run, err
}

// List 分页执行记录（新在前）。
func (s *Service) List(ctx context.Context, limit, offset int) ([]BackupRun, int64, error) {
	var runs []BackupRun
	var total int64
	q := s.db.WithContext(ctx).Model(&BackupRun{})
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if err := s.db.WithContext(ctx).Order("started_at DESC").Limit(limit).Offset(offset).Find(&runs).Error; err != nil {
		return nil, 0, err
	}
	return runs, total, nil
}

// incMetric 安全自增指标（未装配 metrics 时跳过）。
func (s *Service) incMetric(name string, labels ...string) {
	if s.metrics != nil {
		s.metrics.Inc(name, labels...)
	}
}

// BackupState 指标：最近成功且未超过 2×interval → 1，否则 0（无备份/失败/过期）。
func (s *Service) BackupState() float64 {
	last, err := s.LastSuccess(context.Background())
	if err != nil || last == nil || last.FinishedAt == nil {
		return 0
	}
	if time.Since(*last.FinishedAt) > s.cfg.Interval*2 {
		return 0
	}
	return 1
}

// LastSuccessAge 距上次成功备份的秒数（无备份返回 -1）。
func (s *Service) LastSuccessAge() float64 {
	last, err := s.LastSuccess(context.Background())
	if err != nil || last == nil || last.FinishedAt == nil {
		return -1
	}
	return time.Since(*last.FinishedAt).Seconds()
}

// copyFile 跨盘复制（同盘场景系统会自动优化）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// truncateStr rune 安全截断（错误信息/校验详情入库前限长）。
func truncateStr(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
