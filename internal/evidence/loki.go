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

// loki.go：Loki 日志查询客户端（HTTP API /loki/api/v1/query_range）。
// 注意：Loki 的 start/end 是纳秒 unix（与 Prometheus 的秒不同）；direction=backward 取最新。

// LokiClient Loki 数据源客户端。
type LokiClient struct {
	baseURL string
	hc      *http.Client
}

// NewLokiClient 创建客户端。
func NewLokiClient(baseURL string, timeout time.Duration) *LokiClient {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &LokiClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: timeout},
	}
}

// LokiEntry 单条日志。
type LokiEntry struct {
	Timestamp time.Time
	Line      string
}

// LokiResult 查询结果。
type LokiResult struct {
	Entries []LokiEntry
}

// lokiResponse 查询响应（只取需要的字段）。
type lokiResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"` // [ns, line]
		} `json:"result"`
	} `json:"data"`
}

// QueryRange 查询范围日志（start/end 纳秒 unix；limit 钳制 1-1000；direction=backward 最新优先）。
func (c *LokiClient) QueryRange(ctx context.Context, query string, start, end time.Time, limit int) (*LokiResult, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("direction", "backward")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/loki/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("loki: request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loki: call: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("loki: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("loki: status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out lokiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("loki: parse: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("loki: query error: %s", out.Error)
	}
	res := &LokiResult{}
	for _, s := range out.Data.Result {
		for _, v := range s.Values {
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				continue
			}
			res.Entries = append(res.Entries, LokiEntry{
				Timestamp: time.Unix(0, ns),
				Line:      v[1],
			})
		}
	}
	return res, nil
}
