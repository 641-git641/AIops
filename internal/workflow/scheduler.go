package workflow

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"aiops/internal/util"
)

// intervalState interval 触发器调度状态（进程内）：记录每个工作流上次触发时刻，避免同周期重复触发。
type intervalState struct {
	mu   sync.Mutex
	last map[string]time.Time // key: tenantID + "/" + workflowID
}

// StartIntervalScheduler 启动 interval 触发器扫描循环：每 tick 检查一次启用工作流，
// 含 `type: interval` 触发器且到期的立即触发（以进程内 last-fire 防止重复）。
// ctx 取消即停止。tick<=0 默认 10s。
func (s *Service) StartIntervalScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = 10 * time.Second
	}
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.scanInterval(ctx); err != nil {
					slog.Warn("workflow: interval scan", "err", err)
				}
			}
		}
	}()
	slog.Info("workflow: interval scheduler started", "tick", tick)
}

// scanInterval 扫描全部启用工作流，到期的 interval 触发器触发一次运行。
// last-fire 键含 trigger 下标：同一工作流多个 interval 触发器各自独立计时。
// 扫描结束时清理已删除工作流/已移除触发器的残留键（防止 map 无限增长）。
func (s *Service) scanInterval(ctx context.Context) error {
	var wfs []Workflow
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).Find(&wfs).Error; err != nil {
		return err
	}
	now := time.Now().UTC()
	seen := make(map[string]bool)
	for i := range wfs {
		wf := &wfs[i]
		def, err := Parse([]byte(wf.YAML))
		if err != nil {
			slog.Warn("workflow: interval scan parse", "workflow", wf.ID, "err", err)
			continue
		}
		if def.Workflow.Disabled {
			continue
		}
		for ti, tr := range def.Workflow.Triggers {
			if strings.TrimSpace(tr.Type) != "interval" {
				continue
			}
			d, ok := tr.Interval()
			if !ok {
				slog.Warn("workflow: interval trigger missing with.interval", "workflow", wf.ID)
				continue
			}
			key := wf.TenantID + "/" + wf.ID + "/" + strconv.Itoa(ti)
			seen[key] = true
			s.sched.mu.Lock()
			last, exists := s.sched.last[key]
			if !exists {
				// 首次见到：以当前时刻为基准，下一周期再触发（避免重启/建流立即突发）
				s.sched.last[key] = now
				s.sched.mu.Unlock()
				continue
			}
			if now.Sub(last) >= d {
				s.sched.last[key] = now
				s.sched.mu.Unlock()
				s.runInterval(ctx, wf, def, now)
			} else {
				s.sched.mu.Unlock()
			}
		}
	}
	s.sched.mu.Lock()
	for k := range s.sched.last {
		if !seen[k] {
			delete(s.sched.last, k)
		}
	}
	s.sched.mu.Unlock()
	return nil
}

// runInterval 为一次 interval 触发落库运行记录（dedup_key 按触发时刻唯一，不互相防重）并异步执行。
func (s *Service) runInterval(ctx context.Context, wf *Workflow, def *Def, fireAt time.Time) {
	run := &WorkflowRun{
		ID:         util.NewID(),
		TenantID:   wf.TenantID,
		WorkflowID: wf.ID,
		DedupKey:   util.SHA256("interval:" + wf.ID + ":" + fireAt.Format(time.RFC3339Nano)),
		Trigger:    "interval",
		Status:     RunRunning,
		StartedAt:  fireAt,
	}
	if err := s.db.WithContext(ctx).Create(run).Error; err != nil {
		slog.Warn("workflow: interval run insert", "workflow", wf.ID, "err", err)
		return
	}
	s.launch(ctx, run, def, nil)
}
