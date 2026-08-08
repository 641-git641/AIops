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

// batchKey 批内去重键（(tenant, fingerprint)，与 last_alerts 唯一索引一致）。
type batchKey struct {
	tenantID    string
	fingerprint string
}

// batchCandidate 批内待分类事件。fp/hash 用于分类；finalStatus 为该事件写入后
// last_alerts 的最终状态（维护窗口静默事件为 suppressed，与管线设置一致）。
type batchCandidate struct {
	tenant       string
	fp           string
	alertHash    string
	finalStatus  string
	windowActive bool // 维护窗口期内（与 Classify 的 windowActive 同语义）
}

// classifyBatchLast 批量去重分类（Phase 3g 批内 overlay）：基于 DB 批量读到的
// 存量 base + 批内 overlay（前序事件的写入意图）逐条分类，与逐事件 Classify 读
// DB 的语义一致——批量写入尚未落库，overlay 模拟「前序事件已写」后的 last_alerts
// 状态（同批同 fingerprint 的后序事件必须看到前序状态，禁止裸延迟 flush）。
// 返回逐事件 kind；纯函数，无 DB 依赖，便于单测。
func classifyBatchLast(base map[batchKey]*alert.LastAlert, cs []batchCandidate) []alert.DedupKind {
	overlay := make(map[batchKey]*alert.LastAlert, len(base))
	kinds := make([]alert.DedupKind, len(cs))
	for i, c := range cs {
		key := batchKey{c.tenant, c.fp}
		cur, ok := overlay[key]
		if !ok {
			cur = base[key] // nil = 批前无此告警
		}
		if cur == nil {
			kinds[i] = alert.DedupNew
		} else {
			kinds[i] = classifyLastAlert(cur, c.alertHash, c.windowActive)
		}
		if kinds[i] != alert.DedupDuplicate {
			// 该事件将写入：记录写入意图，批内后序同 fingerprint 事件可见
			overlay[key] = &alert.LastAlert{AlertHash: c.alertHash, Status: c.finalStatus}
		}
	}
	return kinds
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
