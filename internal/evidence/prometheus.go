package evidence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// prometheus.go：Prometheus 指标查询客户端（HTTP API /api/v1/query_range）。
// 风格对齐 internal/embedding/openai_compat.go：整体超时、LimitReader、状态码结构化错误。

// PromClient Prometheus 数据源客户端。
type PromClient struct {
	baseURL string
	user    string
	pass    string
	hc      *http.Client // Timeout 覆盖整体调用超时
}

// NewPromClient 创建客户端（user 非空时启用 Basic Auth）。
func NewPromClient(baseURL, user, password string, timeout time.Duration) *PromClient {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &PromClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		user:    user, pass: password,
		hc: &http.Client{Timeout: timeout},
	}
}

// PromSeries 单条指标序列。
type PromSeries struct {
	Labels map[string]string // metric 标签（去重后）
	Points [][2]float64      // [ts(unix 秒), value]
}

// PromResult 查询结果。
type PromResult struct {
	Series      []PromSeries
	SampleCount int
}

// promResponse 查询响应（只取需要的字段）。
type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"` // [ts, "1.23"]——ts 可能是 float64 或 string
		} `json:"result"`
	} `json:"data"`
}

// QueryRange 查询范围指标（start/end 转 unix 秒；step 钳制 ≥15s 防拖垮实例）。
func (c *PromClient) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (*PromResult, error) {
	if step < 15*time.Second {
		step = 15 * time.Second
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.FormatInt(int64(step.Seconds()), 10))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("prometheus: request: %w", err)
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: call: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("prometheus: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus: status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out promResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("prometheus: parse: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus: query error: %s", out.Error)
	}
	res := &PromResult{}
	for _, s := range out.Data.Result {
		ps := PromSeries{Labels: s.Metric}
		for _, v := range s.Values {
			ts, ok := toFloat(v[0])
			if !ok {
				continue
			}
			val, ok := toFloat(v[1])
			if !ok {
				continue
			}
			ps.Points = append(ps.Points, [2]float64{ts, val})
		}
		if len(ps.Points) > 0 {
			res.Series = append(res.Series, ps)
			res.SampleCount += len(ps.Points)
		}
	}
	return res, nil
}

// toFloat 兼容 JSON 数值（float64）与字符串数值。
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
