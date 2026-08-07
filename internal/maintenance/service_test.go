package maintenance

import (
	"encoding/json"
	"testing"

	"aiops/internal/alert"
)

// buildAlert 构造带 labels 的测试告警。
func buildAlert(t *testing.T, fingerprint, sev, status, service string) *alert.Alert {
	t.Helper()
	labels := map[string]any{}
	if service != "" {
		labels["service"] = service
	}
	b, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	return &alert.Alert{
		Fingerprint: fingerprint,
		Severity:    sev,
		Status:      status,
		Labels:      b,
	}
}

func TestMatches(t *testing.T) {
	// 基准告警：pay 服务 high 告警
	payHigh := buildAlert(t, "prometheus:pay-slow", alert.SeverityHigh, alert.StatusFiring, "pay")

	cases := []struct {
		name string
		win  Window
		al   *alert.Alert
		want bool
	}{
		{
			name: "firing + 无任何条件 → 不命中（窗口必须至少一个条件）",
			win:  Window{Name: "x", Enabled: true}, al: payHigh, want: false,
		},
		{
			name: "fingerprint 精确命中",
			win:  Window{Fingerprint: "prometheus:pay-slow"}, al: payHigh, want: true,
		},
		{
			name: "fingerprint 不匹配",
			win:  Window{Fingerprint: "prometheus:pay-down"}, al: payHigh, want: false,
		},
		{
			name: "service 命中",
			win:  Window{Service: "pay"}, al: payHigh, want: true,
		},
		{
			name: "service 不匹配",
			win:  Window{Service: "order"}, al: payHigh, want: false,
		},
		{
			name: "severity 上限覆盖：high <= high",
			win:  Window{Severity: alert.SeverityHigh}, al: payHigh, want: true,
		},
		{
			name: "severity 上限覆盖：low 窗口不覆盖 high",
			win:  Window{Severity: alert.SeverityLow}, al: payHigh, want: false,
		},
		{
			name: "service + severity 全部满足才命中",
			win:  Window{Service: "pay", Severity: alert.SeverityHigh}, al: payHigh, want: true,
		},
		{
			name: "service 满足但 severity 超限 → 不命中",
			win:  Window{Service: "pay", Severity: alert.SeverityMedium}, al: payHigh, want: false,
		},
		{
			name: "resolved 告警一律放行（即使条件全部匹配）",
			win:  Window{Service: "pay"},
			al:   buildAlert(t, "prometheus:pay-slow", alert.SeverityHigh, alert.StatusResolved, "pay"),
			want: false,
		},
		{
			name: "suppressed 状态不重复静默（保持幂等）",
			win:  Window{Service: "pay"},
			al:   buildAlert(t, "prometheus:pay-slow", alert.SeverityHigh, alert.StatusSuppressed, "pay"),
			want: false,
		},
		{
			name: "severity=critical 窗口覆盖一切",
			win:  Window{Severity: alert.SeverityCritical},
			al:   buildAlert(t, "x", alert.SeverityLow, alert.StatusFiring, ""),
			want: true,
		},
		{
			name: "labels 无 service 时 service 条件不匹配",
			win:  Window{Service: "pay"},
			al:   buildAlert(t, "x", alert.SeverityHigh, alert.StatusFiring, ""),
			want: false,
		},
	}

	svc := &Service{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := svc.Matches(&tc.win, tc.al); got != tc.want {
				t.Errorf("Matches() = %v, want %v", got, tc.want)
			}
		})
	}
}
