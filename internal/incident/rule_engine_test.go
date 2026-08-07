package incident

import (
	"encoding/json"
	"testing"

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
