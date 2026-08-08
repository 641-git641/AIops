package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeBaseline 写一个固定 profile 的基线汇总文件（可 mutate 制造不同场景）。
func writeBaseline(t *testing.T, dir string, mutate func(*summary)) string {
	t.Helper()
	sm := summary{
		Run:        runProfile{Rate: 100, DurationS: 2, Concurrency: 4, Variety: 20, Batch: 1, Severity: "medium"},
		Throughput: 100, P50: 10, P99: 20,
		ElapsedS: 2, Sent: 200, OK: 200,
	}
	if mutate != nil {
		mutate(&sm)
	}
	raw, err := json.MarshalIndent(sm, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "baseline.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 本次压测结果：profile 与基线一致，指标与基线相同。
var testResult = summary{
	Run:        runProfile{Rate: 100, DurationS: 2, Concurrency: 4, Variety: 20, Batch: 1, Severity: "medium"},
	Throughput: 100, P50: 10, P99: 20,
	ElapsedS: 2, Sent: 200, OK: 200,
}

func TestCompareWithBaseline(t *testing.T) {
	t.Run("同指标无回归", func(t *testing.T) {
		b := writeBaseline(t, t.TempDir(), nil)
		if compareWithBaseline(b, testResult) {
			t.Fatal("相同指标不应判回归")
		}
	})
	t.Run("吞吐退化", func(t *testing.T) {
		// 基线吞吐 1000 → 本次 100 < 800（0.8×）判回归
		b := writeBaseline(t, t.TempDir(), func(s *summary) { s.Throughput = 1000 })
		if !compareWithBaseline(b, testResult) {
			t.Fatal("吞吐 -90% 应判回归")
		}
	})
	t.Run("延迟退化", func(t *testing.T) {
		// 基线 p50 5 / p99 10 → 本次 10/20 > 1.5× 判回归
		b := writeBaseline(t, t.TempDir(), func(s *summary) { s.P50, s.P99 = 5, 10 })
		if !compareWithBaseline(b, testResult) {
			t.Fatal("延迟 2× 应判回归")
		}
	})
	t.Run("profile不一致判失败", func(t *testing.T) {
		b := writeBaseline(t, t.TempDir(), func(s *summary) { s.Run.Rate = 999 })
		if !compareWithBaseline(b, testResult) {
			t.Fatal("profile 不一致的比较无意义，应判失败")
		}
	})
	t.Run("基线缺失判失败", func(t *testing.T) {
		if !compareWithBaseline(filepath.Join(t.TempDir(), "nope.json"), testResult) {
			t.Fatal("基线缺失应判失败（提示先 make regress-baseline）")
		}
	})
}
