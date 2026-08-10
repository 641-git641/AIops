package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OpenAICompat OpenAI 兼容 /embeddings 客户端（标准库 http，风格对齐 internal/ai/llm.go）。
// baseURL 可直接指向 /embeddings 或根地址（自动拼接）。
type OpenAICompat struct {
	baseURL string
	apiKey  string
	model   string
	hc      *http.Client
	batch   int

	mu        sync.Mutex
	dimension int // 首次响应学习；0=未知
}

// NewOpenAICompat 创建 embedding 客户端。
func NewOpenAICompat(baseURL, apiKey, model string, timeout time.Duration, batchSize int) *OpenAICompat {
	if batchSize <= 0 {
		batchSize = 32
	}
	return &OpenAICompat{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		hc:      &http.Client{Timeout: timeout},
		batch:   batchSize,
	}
}

// SetTimeout 调整整体超时（测试/装配用）。
func (c *OpenAICompat) SetTimeout(d time.Duration) { c.hc.Timeout = d }

// embedURL 解析请求端点：/embeddings 或根地址自动拼接。
func (c *OpenAICompat) embedURL() string {
	u := strings.TrimRight(c.baseURL, "/")
	if strings.HasSuffix(u, "/embeddings") {
		return u
	}
	return u + "/embeddings"
}

// ModelID 模型标识。
func (c *OpenAICompat) ModelID() string { return c.model }

// Dimension 已学习维度；0=未知。
func (c *OpenAICompat) Dimension() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dimension
}

// EmbedDocuments 批量向量化（自动按 batch 分片）。
func (c *OpenAICompat) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.batch {
		end := min(start+c.batch, len(texts))
		vs, err := c.embed(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	return out, nil
}

// EmbedQuery 单条查询向量化。
func (c *OpenAICompat) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vs, err := c.embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vs[0], nil
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// embed 单次请求（批内并发安全；错误带状态信息，非 JSON 错误体也返回）。
func (c *OpenAICompat) embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.embedURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding: call: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("embedding: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding: status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out embedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embedding: parse: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("embedding: api error: %s", out.Error.Message)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embedding: response count %d != input %d", len(out.Data), len(texts))
	}
	vs := make([][]float32, 0, len(out.Data))
	for _, d := range out.Data {
		if len(d.Embedding) == 0 {
			return nil, errors.New("embedding: empty vector in response")
		}
		vs = append(vs, d.Embedding)
	}
	c.mu.Lock()
	if c.dimension == 0 {
		c.dimension = len(vs[0])
	} else if c.dimension != len(vs[0]) {
		c.mu.Unlock()
		return nil, fmt.Errorf("embedding: dimension drift %d -> %d（换模型必须新建索引版本）", c.dimension, len(vs[0]))
	}
	c.mu.Unlock()
	return vs, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
