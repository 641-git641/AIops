package pipeline

import (
	"context"

	"aiops/internal/alert"
)

// LastAlertReader 去重热路径所需的 last_alerts 只读能力（仓储实现；接口便于单测注入）。
type LastAlertReader interface {
	GetLastAlert(ctx context.Context, tenantID, fingerprint string) (*alert.LastAlert, error)
}

// Deduper 两级去重：
//   - fingerprint（逻辑键）：同名同源告警视为同一逻辑告警
//   - alert_hash（物理键）：内容哈希，相同 = 完全重复丢弃；不同 = 状态/内容刷新
//
// 参考 keep 的 AlertDeduplicator 与 multi-agent-aiops 的 AlertFingerprint。
type Deduper struct {
	repo LastAlertReader
}

// NewDeduper 创建去重器。
func NewDeduper(repo LastAlertReader) *Deduper {
	return &Deduper{repo: repo}
}

// Classify 分类：new（新告警）/ update（内容变化刷新）/ duplicate（完全重复丢弃）。
// windowActive 为当前是否处于维护窗口生效期：窗口内 suppressed 同内容视为重复
// （避免窗口期心跳刷表）；窗口失效后 suppressed 是过期状态，须重新上浮为 firing。
//
// 单读热路径：last_alerts 冗余了 alert_hash + status（写入路径 SaveAlert CTE /
// UpsertLastAlert 同步维护），只查 (tenant, fingerprint) 唯一索引 1 次即完成分类，
// 不再回读 alerts 分区表（Phase 3c 之后的下一级读写合并）。
func (d *Deduper) Classify(ctx context.Context, tenantID, fingerprint, alertHash string, windowActive bool) (alert.DedupKind, error) {
	la, err := d.repo.GetLastAlert(ctx, tenantID, fingerprint)
	if err != nil {
		return "", err
	}
	if la == nil {
		return alert.DedupNew, nil
	}
	return classifyLastAlert(la, alertHash, windowActive), nil
}

// classifyLastAlert 纯函数：基于 last_alerts 状态缓存分类（与 DB 解耦，便于单测）。
// 以 last_alerts 为当前状态真源：alerts 历史行可能已被保留策略删除，但状态缓存
// 仍准确（firing + 同内容 = 完全重复）。已恢复（resolved/suppressed 非窗口期）的
// 告警同内容复发须重新触发（非 duplicate）。
func classifyLastAlert(la *alert.LastAlert, alertHash string, windowActive bool) alert.DedupKind {
	if la.AlertHash == alertHash {
		if la.Status == alert.StatusFiring {
			return alert.DedupDuplicate
		}
		if la.Status == alert.StatusSuppressed && windowActive {
			return alert.DedupDuplicate
		}
	}
	return alert.DedupUpdate
}
