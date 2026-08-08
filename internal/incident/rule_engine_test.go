package incident

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"aiops/internal/alert"

	"gorm.io/datatypes"
)

func mustJSON(v any) datatypes.JSON {
	b, _ := json.Marshal(v)
	return b
}

func TestGroupingKeyGroupsByStableFields(t *testing.T) {
	r := &Rule{GroupingCriteria: mustJSON([]string{"provider", "labels.service"})}
	a1 := &alert.Alert{Provider: "prometheus", Labels: mustJSON(map[string]any{"service": "pay"})}
	a2 := &alert.Alert{Provider: "prometheus", Labels: mustJSON(map[string]any{"service": "pay"})}
	a3 := &alert.Alert{Provider: "prometheus", Labels: mustJSON(map[string]any{"service": "order"})}

	k1 := groupingKey(r, a1)
	k2 := groupingKey(r, a2)
	k3 := groupingKey(r, a3)

	if k1 != k2 {
		t.Fatal("same group fields must produce same key")
	}
	if k1 == k3 {
		t.Fatal("different group fields must produce different keys")
	}
}

func TestGroupingKeyAllWhenNoCriteria(t *testing.T) {
	r := &Rule{GroupingCriteria: mustJSON([]string{})}
	a1 := &alert.Alert{Provider: "prometheus", Name: "disk-full"}
	if k := groupingKey(r, a1); k == "" {
		t.Fatal("grouping key must not be empty")
	}
}

func TestGroupingKeyMissingLabel(t *testing.T) {
	// 标签缺失时该字段按空串参与哈希，不 panic
	r := &Rule{GroupingCriteria: mustJSON([]string{"labels.host"})}
	a := &alert.Alert{Provider: "prometheus", Labels: mustJSON(map[string]any{"service": "pay"})}
	if k := groupingKey(r, a); k == "" {
		t.Fatal("grouping key must not be empty")
	}
}

// -------- 规则缓存（enabledRules）--------

// fakeRuleEngine 构造一个可用假 loader 与假时钟驱动的 RuleEngine（不触 DB）。
func fakeRuleEngine(ttl time.Duration) (*RuleEngine, *int, func(time.Time)) {
	re := NewRuleEngine(nil, nil, nil, ttl)
	calls := 0
	re.loadRules = func(_ context.Context, tenant string) ([]Rule, error) {
		calls++
		return []Rule{{ID: tenant + "-r", CELExpression: "true", Enabled: true}}, nil
	}
	base := time.Now()
	re.now = func() time.Time { return base }
	return re, &calls, func(t time.Time) { base = t }
}

func TestRuleCacheHitWithinTTL(t *testing.T) {
	re, calls, _ := fakeRuleEngine(time.Second)
	got, err := re.enabledRules(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "t1-r" {
		t.Fatalf("want [t1-r], got %+v", got)
	}
	if *calls != 1 {
		t.Fatalf("first call should fetch, calls=%d", *calls)
	}
	// TTL 内再次取 → 命中缓存，不再查库
	if _, err := re.enabledRules(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("cache hit should not refetch, calls=%d", *calls)
	}
}

func TestRuleCacheExpiresAfterTTL(t *testing.T) {
	re, calls, advance := fakeRuleEngine(time.Second)
	if _, err := re.enabledRules(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	// 超过 TTL 后再次取 → 重新查库
	advance(time.Now().Add(2 * time.Second))
	if _, err := re.enabledRules(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("expired cache should refetch, calls=%d", *calls)
	}
}

func TestRuleCacheInvalidate(t *testing.T) {
	re, calls, _ := fakeRuleEngine(time.Second)
	if _, err := re.enabledRules(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	re.Invalidate("t1")
	// 失效后即使 TTL 内也重新查库；其他租户缓存不受影响
	if _, err := re.enabledRules(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("invalidate should force refetch for that tenant, calls=%d", *calls)
	}
	if _, err := re.enabledRules(context.Background(), "t2"); err != nil {
		t.Fatal(err)
	}
	if *calls != 3 {
		t.Fatalf("other tenant should fetch its own rules, calls=%d", *calls)
	}
}

func TestRuleCacheLoaderErrorDoesNotPoison(t *testing.T) {
	re, _, _ := fakeRuleEngine(time.Second)
	calls := 0
	re.loadRules = func(_ context.Context, tenant string) ([]Rule, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("db down")
		}
		return []Rule{{ID: tenant + "-r", CELExpression: "true"}}, nil
	}
	if _, err := re.enabledRules(context.Background(), "t1"); err == nil {
		t.Fatal("loader error should propagate")
	}
	// 错误不写入缓存 → 下次成功可正常缓存，且之后命中缓存
	rules, err := re.enabledRules(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("want 1 rule after recovery, got %d", len(rules))
	}
	if calls != 2 {
		t.Fatalf("loader should run again after failure, calls=%d", calls)
	}
	if _, err := re.enabledRules(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("cached success should not refetch, calls=%d", calls)
	}
}

func TestRuleCacheConcurrentSafe(t *testing.T) {
	// 并发访问不 panic、不丢缓存（-race 校验锁正确性；double-check 收敛回填）
	re, _, _ := fakeRuleEngine(time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := re.enabledRules(context.Background(), "t1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	re.ruleMu.RLock()
	defer re.ruleMu.RUnlock()
	if _, ok := re.ruleCache["t1"]; !ok {
		t.Fatal("cache should be populated after concurrent access")
	}
}
