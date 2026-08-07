package metrics

import (
	"strings"
	"testing"
)

func TestRegistryWriteToPrometheusText(t *testing.T) {
	r := New()
	r.Counter("aiops_alerts_total", "入库告警数", "kind")
	r.Counter("aiops_alerts_duplicated_total", "完全重复被丢弃的告警数")
	r.Histogram("aiops_pipeline_duration_seconds", "管线耗时", SecondsBuckets)
	r.Gauge("aiops_queue_depth", "队列深度", func() float64 { return 3.5 })

	r.Inc("aiops_alerts_total", "new")
	r.Inc("aiops_alerts_total", "new")
	r.Inc("aiops_alerts_total", "update")
	r.Inc("aiops_alerts_duplicated_total")
	r.Observe("aiops_pipeline_duration_seconds", 0.002)
	r.Observe("aiops_pipeline_duration_seconds", 0.03)

	var sb strings.Builder
	if err := r.Write(&sb); err != nil {
		t.Fatalf("WriteTo error = %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"# TYPE aiops_alerts_total counter",
		`aiops_alerts_total{kind="new"} 2`,
		`aiops_alerts_total{kind="update"} 1`,
		"aiops_alerts_duplicated_total 1",
		"# TYPE aiops_pipeline_duration_seconds histogram",
		`aiops_pipeline_duration_seconds_bucket{le="0.001"} 0`,
		`aiops_pipeline_duration_seconds_bucket{le="0.005"} 1`,
		`aiops_pipeline_duration_seconds_bucket{le="0.05"} 2`,
		`aiops_pipeline_duration_seconds_bucket{le="+Inf"} 2`,
		"aiops_pipeline_duration_seconds_sum 0.032",
		"aiops_pipeline_duration_seconds_count 2",
		"# TYPE aiops_queue_depth gauge",
		"aiops_queue_depth 3.5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestIncUnknownMetricNoPanic(t *testing.T) {
	r := New()
	r.Inc("not_registered") // 未注册指标静默忽略（埋点不阻塞业务）
	r.Observe("not_registered", 1)
	var sb strings.Builder
	if err := r.Write(&sb); err != nil {
		t.Fatalf("WriteTo error = %v", err)
	}
	if sb.Len() != 0 {
		t.Errorf("unexpected output for empty registry: %q", sb.String())
	}
}
