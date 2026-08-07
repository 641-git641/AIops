package pipeline

import (
	"context"

	"aiops/internal/alert"
)

// Deduper 两级去重：
//   - fingerprint（逻辑键）：同名同源告警视为同一逻辑告警
//   - alert_hash（物理键）：内容哈希，相同 = 完全重复丢弃；不同 = 状态/内容刷新
//
// 参考 keep 的 AlertDeduplicator 与 multi-agent-aiops 的 AlertFingerprint。
type Deduper struct {
	repo *alert.Repository
}

// NewDeduper 创建去重器。
func NewDeduper(repo *alert.Repository) *Deduper {
	return &Deduper{repo: repo}
}

// Classify 分类：new（新告警）/ update（内容变化刷新）/ duplicate（完全重复丢弃）。
// windowActive 为当前是否处于维护窗口生效期：窗口内 suppressed 同内容视为重复
// （避免窗口期心跳刷表）；窗口失效后 suppressed 是过期状态，须重新上浮为 firing。
func (d *Deduper) Classify(ctx context.Context, tenantID, fingerprint, alertHash string, windowActive bool) (alert.DedupKind, error) {
	la, err := d.repo.GetLastAlert(ctx, tenantID, fingerprint)
	if err != nil {
		return "", err
	}
	if la == nil {
		return alert.DedupNew, nil
	}
	if la.AlertID != "" {
		// 与当前活跃告警比较内容哈希；已恢复的告警同内容复发须重新触发（非 duplicate）。
		// firing（或窗口期内的 suppressed）且内容相同 → 完全重复，避免刷表。
		cur, err := d.repo.Get(ctx, tenantID, la.AlertID)
		if err == nil && cur != nil && cur.AlertHash == alertHash {
			if cur.Status == alert.StatusFiring {
				return alert.DedupDuplicate, nil
			}
			if cur.Status == alert.StatusSuppressed && windowActive {
				return alert.DedupDuplicate, nil
			}
		}
	}
	return alert.DedupUpdate, nil
}
