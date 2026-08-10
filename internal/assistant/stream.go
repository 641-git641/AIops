package assistant

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"aiops/internal/util"

	"gorm.io/gorm"
)

// stream.go：run 级事件流（AGENT_CHAT_PLAN §6.6/§7）。
// 事件先持久化（DB 原子分配 sequence）再内存 fanout——断线重连按 lastSequence
// 从 DB 快照恢复，sequence 单调递增（同 run 单写者，无并发冲突）。

// Event 单个运行事件（SSE 线格式与 DB 快照共用）。
type Event struct {
	RunID     string          `json:"runId"`
	Sequence  int64           `json:"sequence"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
}

// Broker 事件分发：DB 持久化 + 内存订阅（单实例部署前提，AGENT_CHAT_PLAN §16.5）。
type Broker struct {
	db   *gorm.DB
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{} // runID → 订阅者
}

// NewBroker 创建事件 broker。
func NewBroker(db *gorm.DB) *Broker {
	return &Broker{db: db, subs: map[string]map[chan Event]struct{}{}}
}

// Publish 持久化事件并 fanout。sequence 由 SQL 原子分配（重启安全）。
// 同 run 单写者（仅 runner worker 发布），无并发 sequence 冲突。
func (b *Broker) Publish(ctx context.Context, runID, tenantID, typ string, data any) (int64, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	ev := Event{RunID: runID, Type: typ, Data: payload, Timestamp: time.Now().UTC()}
	// sequence 由 SQL 原子分配；用底层 sql.DB 拿 RETURNING（GORM Raw 对 INSERT 不解析返回行）
	sqlDB, err := b.db.DB()
	if err != nil {
		return 0, err
	}
	// 底层 sql.DB 用 pg 原生 $n 占位符（pgx stdlib 对 `?::jsonb`/CAST(? AS jsonb) 的转换不可靠）
	err = sqlDB.QueryRowContext(ctx, `
		INSERT INTO assistant_events (id, tenant_id, run_id, sequence, type, data, created_at)
		SELECT $1, $2, $3, COALESCE(MAX(sequence), 0) + 1, $4, CAST($5 AS jsonb), now()
		FROM assistant_events WHERE run_id = $6
		RETURNING sequence`,
		util.NewID(), tenantID, runID, typ, string(payload), runID).Scan(&ev.Sequence)
	if err != nil {
		return 0, err
	}
	// 内存 fanout（非阻塞：慢消费者丢事件，靠快照恢复）
	b.mu.Lock()
	for ch := range b.subs[runID] {
		select {
		case ch <- ev:
		default:
		}
	}
	b.mu.Unlock()
	return ev.Sequence, nil
}

// Subscribe 订阅 run 的实时事件（返回取消函数）。
func (b *Broker) Subscribe(runID string) (<-chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	if b.subs[runID] == nil {
		b.subs[runID] = map[chan Event]struct{}{}
	}
	b.subs[runID][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if m := b.subs[runID]; m != nil {
			delete(m, ch)
			if len(m) == 0 {
				delete(b.subs, runID)
			}
		}
		b.mu.Unlock()
	}
}

// EventsSince 读取 run 的 DB 事件快照（sequence > lastSeq，升序；断线续传）。
func (b *Broker) EventsSince(ctx context.Context, tenantID, runID string, lastSeq int64) ([]Event, error) {
	rows, err := b.db.WithContext(ctx).Raw(`
		SELECT run_id, sequence, type, data, created_at
		FROM assistant_events
		WHERE tenant_id = ? AND run_id = ? AND sequence > ?
		ORDER BY sequence ASC
		LIMIT 5000`, tenantID, runID, lastSeq).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var data []byte
		var ts time.Time
		if err := rows.Scan(&ev.RunID, &ev.Sequence, &ev.Type, &data, &ts); err != nil {
			return nil, err
		}
		ev.Data = data
		ev.Timestamp = ts
		out = append(out, ev)
	}
	return out, rows.Err()
}
