package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeEmbedServer 记录请求并返回确定性向量。
func fakeEmbedServer(t *testing.T, dim int, fn func(r embedRequest)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			http.Error(w, "bad path "+r.URL.Path, http.StatusNotFound)
			return
		}
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if fn != nil {
			fn(req)
		}
		resp := embedResponse{}
		for i, in := range req.Input {
			vec := make([]float32, dim)
			vec[i%dim] = float32(len(in)) // 确定性但可区分
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: vec})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestOpenAICompatEmbedQuery(t *testing.T) {
	got := map[string]string{}
	srv := fakeEmbedServer(t, 4, func(r embedRequest) {
		got["model"] = r.Model
		got["input"] = strings.Join(r.Input, "|")
	})
	defer srv.Close()

	c := NewOpenAICompat(srv.URL, "key-1", "test-embed", 3*time.Second, 4)
	vec, err := c.EmbedQuery(context.Background(), "连接池耗尽")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 4 {
		t.Fatalf("dim = %d, want 4", len(vec))
	}
	if got["model"] != "test-embed" || !strings.Contains(got["input"], "连接池耗尽") {
		t.Fatalf("request mismatch: %v", got)
	}
	if c.Dimension() != 4 {
		t.Fatalf("learned dimension = %d, want 4", c.Dimension())
	}
}

func TestOpenAICompatRootURLJoins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2,3]}]}`))
	}))
	defer srv.Close()

	c := NewOpenAICompat(srv.URL+"/v1", "", "m", time.Second, 4)
	vec, err := c.EmbedQuery(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 3 {
		t.Fatalf("dim = %d, want 3", len(vec))
	}
}

func TestOpenAICompatBatch(t *testing.T) {
	var batchSizes []int
	srv := fakeEmbedServer(t, 2, func(r embedRequest) { batchSizes = append(batchSizes, len(r.Input)) })
	defer srv.Close()

	c := NewOpenAICompat(srv.URL, "", "m", time.Second, 3)
	texts := make([]string, 7)
	for i := range texts {
		texts[i] = "t" + strings.Repeat("x", i)
	}
	vs, err := c.EmbedDocuments(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 7 {
		t.Fatalf("got %d vectors, want 7", len(vs))
	}
	if len(batchSizes) != 3 || batchSizes[0] != 3 || batchSizes[1] != 3 || batchSizes[2] != 1 {
		t.Fatalf("batch split = %v, want [3 3 1]", batchSizes)
	}
}

func TestOpenAICompatErrors(t *testing.T) {
	t.Run("non-2xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		}))
		defer srv.Close()
		c := NewOpenAICompat(srv.URL, "", "m", time.Second, 4)
		if _, err := c.EmbedQuery(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("want status error, got %v", err)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":`))
		}))
		defer srv.Close()
		c := NewOpenAICompat(srv.URL, "", "m", time.Second, 4)
		if _, err := c.EmbedQuery(context.Background(), "x"); err == nil {
			t.Fatal("want parse error")
		}
	})
	t.Run("count mismatch", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer srv.Close()
		c := NewOpenAICompat(srv.URL, "", "m", time.Second, 4)
		if _, err := c.EmbedQuery(context.Background(), "x"); err == nil {
			t.Fatal("want count error")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(200 * time.Millisecond)
		}))
		defer srv.Close()
		c := NewOpenAICompat(srv.URL, "", "m", 50*time.Millisecond, 4)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := c.EmbedQuery(ctx, "x"); err == nil {
			t.Fatal("want timeout error")
		}
	})
	t.Run("auth header", func(t *testing.T) {
		var auth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"embedding":[1]}]}`))
		}))
		defer srv.Close()
		c := NewOpenAICompat(srv.URL, "secret-key", "m", time.Second, 4)
		if _, err := c.EmbedQuery(context.Background(), "x"); err != nil {
			t.Fatal(err)
		}
		if auth != "Bearer secret-key" {
			t.Fatalf("auth = %q", auth)
		}
	})
}

func TestMockDeterministic(t *testing.T) {
	m := NewMock(16)
	v1, _ := m.EmbedQuery(context.Background(), "order 连接池耗尽")
	v2, _ := m.EmbedQuery(context.Background(), "order 连接池耗尽")
	v3, _ := m.EmbedQuery(context.Background(), "pay 磁盘写满")
	if len(v1) != 16 {
		t.Fatalf("dim = %d", len(v1))
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatal("same text must yield same vector")
		}
	}
	same := 0
	for i := range v1 {
		if v1[i] == v3[i] {
			same++
		}
	}
	if same == 16 {
		t.Fatal("different text must differ")
	}
	if m.ModelID() == "" {
		t.Fatal("model id empty")
	}
}
