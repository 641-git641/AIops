package anomaly

import (
	"math"
	"testing"
)

// meanStd 均值/样本标准差。
func TestMeanStd(t *testing.T) {
	mean, std := meanStd([]int{})
	if mean != 0 || std != 0 {
		t.Fatalf("空样本应全 0: %f %f", mean, std)
	}
	mean, std = meanStd([]int{5})
	if mean != 5 || std != 0 {
		t.Fatalf("单样本 std 应 0: %f %f", mean, std)
	}
	mean, std = meanStd([]int{0, 2, 4}) // 均值 2，样本标准差 2
	if math.Abs(mean-2) > 1e-9 || math.Abs(std-2) > 1e-9 {
		t.Fatalf("mean=%f std=%f，want 2/2", mean, std)
	}
}

// 区间展示：整数省略小数。
func TestFmtRange(t *testing.T) {
	if got := fmtRange(1, 3); got != "[1, 3]" {
		t.Fatalf("got %s", got)
	}
	if got := fmtRange(1.5, 3.5); got != "[1.5, 3.5]" {
		t.Fatalf("got %s", got)
	}
}
