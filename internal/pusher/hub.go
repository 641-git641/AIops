// Package pusher 实现实时推送：自研 WebSocket hub（~150 行）。
// 参考 keep 的 pusher：后端 Broadcast 结构化事件，前端 WS 消费刷新。
// 扩展点：Broadcaster 接口保留替换为 soketi/Pusher 协议的余地。
package pusher

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Event 推送事件。
type Event struct {
	Type     string `json:"type"` // alert.new / alert.update / incident.upsert ...
	TenantID string `json:"-"`    // 内部按租户路由，不序列化
	Data     any    `json:"data"`
}

// Broadcaster 广播接口（pusher 与外部实现都实现它）。
type Broadcaster interface {
	Broadcast(e Event)
}

// client 单个 WS 连接。
type client struct {
	conn   *websocket.Conn
	send   chan []byte
	tenant string
	close  chan struct{}
}

// Hub WS 连接管理。
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
}

// NewHub 创建 hub。
func NewHub() *Hub {
	return &Hub{clients: map[*client]struct{}{}}
}

// Broadcast 向指定租户的连接广播事件（非阻塞，慢消费者丢消息）。
func (h *Hub) Broadcast(e Event) {
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if c.tenant != e.TenantID {
			continue
		}
		select {
		case c.send <- data:
		default: // 发送缓冲区满：丢弃（避免阻塞广播）
			slog.Warn("pusher: drop event to slow client", "tenant", e.TenantID)
		}
	}
}

// ServeWS 升级 HTTP 连接为 WS 客户端。
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, tenantID string) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // 身份校验已在 API 中间件完成
	})
	if err != nil {
		return
	}
	c := &client{
		conn:   conn,
		send:   make(chan []byte, 64),
		tenant: tenantID,
		close:  make(chan struct{}),
	}
	h.add(c)
	go h.writeLoop(c)
	h.readLoop(c) // 阻塞直到连接关闭
	h.remove(c)
	_ = conn.CloseNow()
}

func (h *Hub) add(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	close(c.close)
}

func (h *Hub) writeLoop(c *client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.conn.Write(ctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				return
			}
		case <-c.close:
			return
		}
	}
}

func (h *Hub) readLoop(c *client) {
	// 客户端不应主动发消息；持续读以感知断开，忽略内容
	for {
		_, _, err := c.conn.Read(context.Background())
		if err != nil {
			return
		}
	}
}
