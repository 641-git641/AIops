package circuit

import (
	"testing"
	"time"
)

func TestBreakerClosedAllows(t *testing.T) {
	b := NewBreaker(5, time.Minute)
	if !b.Allow() {
		t.Fatal("closed breaker must allow")
	}
	b.Success()
	if b.State() != StateClosed {
		t.Fatalf("state = %v, want closed", b.State())
	}
	if b.Failures() != 0 {
		t.Fatalf("failures = %d, want 0", b.Failures())
	}
}

func TestBreakerTripsAfterThreshold(t *testing.T) {
	b := NewBreaker(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("allow #%d must pass before trip", i+1)
		}
		b.Failure()
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %v, want open", b.State())
	}
	if b.Allow() {
		t.Fatal("open breaker must block")
	}
}

func TestBreakerRecoversAfterCooldown(t *testing.T) {
	base := time.Now()
	b := NewBreaker(2, 60*time.Second)
	b.now = func() time.Time { return base }

	// 两次失败 → OPEN
	b.Allow()
	b.Failure()
	b.Allow()
	b.Failure()
	if b.State() != StateOpen {
		t.Fatalf("state = %v, want open", b.State())
	}

	// 冷却期内仍阻断
	if b.Allow() {
		t.Fatal("must still block inside cooldown")
	}

	// 冷却期后：半开放行一次试探
	b.now = func() time.Time { return base.Add(61 * time.Second) }
	if !b.Allow() {
		t.Fatal("half-open must allow the probe")
	}
	if b.Allow() {
		t.Fatal("only one probe allowed per half-open window")
	}

	// 试探成功 → 闭合
	b.Success()
	if b.State() != StateClosed {
		t.Fatalf("state = %v, want closed after probe success", b.State())
	}
}

func TestBreakerHalfOpenFailureTripsOpen(t *testing.T) {
	base := time.Now()
	b := NewBreaker(2, 60*time.Second)
	b.now = func() time.Time { return base }
	b.Allow()
	b.Failure()
	b.Allow()
	b.Failure()

	b.now = func() time.Time { return base.Add(61 * time.Second) }
	if !b.Allow() {
		t.Fatal("half-open must allow the probe")
	}
	b.Failure() // 试探失败 → 立即重开
	if b.State() != StateOpen {
		t.Fatalf("state = %v, want open after probe failure", b.State())
	}
	if b.Allow() {
		t.Fatal("must block after probe failure")
	}
}
