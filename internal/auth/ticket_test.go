package auth

import (
	"errors"
	"testing"
	"time"
)

func TestTicketLifecycle(t *testing.T) {
	s := NewTicketStore(time.Second)
	tok, err := s.Issue("default")
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	tenant, err := s.Redeem(tok)
	if err != nil || tenant != "default" {
		t.Fatalf("redeem = %q, %v", tenant, err)
	}
	// 单次使用：第二次兑换必须失败
	if _, err := s.Redeem(tok); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("second redeem = %v, want ErrTicketInvalid", err)
	}
	// 伪造 token
	if _, err := s.Redeem("deadbeef"); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("forged redeem = %v, want ErrTicketInvalid", err)
	}
}

func TestTicketExpiry(t *testing.T) {
	s := NewTicketStore(30 * time.Millisecond)
	tok, _ := s.Issue("default")
	time.Sleep(60 * time.Millisecond)
	if _, err := s.Redeem(tok); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("expired redeem = %v, want ErrTicketInvalid", err)
	}
	// 过期项被惰性清理（map 不增长）
	s.Issue("t1")
	time.Sleep(60 * time.Millisecond)
	_, _ = s.Issue("t2") // 触发 purge
	s.mu.Lock()
	n := len(s.m)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("map size = %d, want 1 (expired purged)", n)
	}
}
