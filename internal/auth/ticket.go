package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// ticket.go：WebSocket 短期访问 ticket（KEEPON 4.3）。
// 浏览器 WS 无法携带自定义 header，故 REST 解锁后签发短时 ticket，握手时经 ?ticket= 校验。
// 30s TTL + 单次使用 + 绑定租户；即使出现在 devtools/访问日志中也已过期（ticket 已进日志脱敏清单）。

// ErrTicketInvalid ticket 不存在/已用/过期。
var ErrTicketInvalid = errors.New("invalid ticket")

type ticket struct {
	tenant    string
	expiresAt time.Time
	used      bool
}

// TicketStore 内存 ticket 存储（不落库，重启即全部失效，前端自动重取）。
type TicketStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]*ticket
}

// NewTicketStore 创建 ticket 存储（ttl 为 ticket 有效期，如 30s）。
func NewTicketStore(ttl time.Duration) *TicketStore {
	return &TicketStore{ttl: ttl, m: make(map[string]*ticket)}
}

// TTL 返回 ticket 有效期。
func (s *TicketStore) TTL() time.Duration { return s.ttl }

// Issue 为租户签发一次性 ticket。
func (s *TicketStore) Issue(tenant string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	s.m[tok] = &ticket{tenant: tenant, expiresAt: time.Now().Add(s.ttl)}
	return tok, nil
}

// Redeem 兑换 ticket（单次使用）：成功返回所属租户，失败返回 ErrTicketInvalid。
func (s *TicketStore) Redeem(token string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	t, ok := s.m[token]
	if !ok || t.used || time.Now().After(t.expiresAt) {
		return "", ErrTicketInvalid
	}
	t.used = true
	delete(s.m, token)
	return t.tenant, nil
}

// purgeLocked 惰性清理过期项（调用方持锁）。
func (s *TicketStore) purgeLocked() {
	now := time.Now()
	for k, t := range s.m {
		if now.After(t.expiresAt) {
			delete(s.m, k)
		}
	}
}
