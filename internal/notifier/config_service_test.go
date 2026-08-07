package notifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// countHandler 统计请求次数（断言未启用/空 webhook 渠道不发送）。
func countHandler(n *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusOK)
	}
}

func TestServiceReloadFromConfigs(t *testing.T) {
	httpHits := &atomic.Int32{}
	dingHits := &atomic.Int32{}
	httpSrv := httptest.NewServer(countHandler(httpHits))
	defer httpSrv.Close()
	dingSrv := httptest.NewServer(countHandler(dingHits))
	defer dingSrv.Close()

	svc := NewService("")
	svc.Reload([]ChannelConfig{
		{Name: ChannelHTTP, Enabled: true, Webhook: httpSrv.URL},
		{Name: ChannelDingTalk, Enabled: false, Webhook: dingSrv.URL}, // 停用 → 不注册
		{Name: ChannelWecom, Enabled: true, Webhook: ""},              // 空 webhook → 不注册
		{Name: "slack", Enabled: true, Webhook: "https://x"},          // 不支持的渠道 → 忽略
	})

	got := svc.Enabled()
	if len(got) != 1 || got[0] != ChannelHTTP {
		t.Fatalf("Enabled = %v, want [http]", got)
	}

	results := svc.Notify(context.Background(), testMsg())
	if len(results) != 1 || !results[0].OK {
		t.Errorf("results = %v, want 1 ok", results)
	}
	if httpHits.Load() != 1 {
		t.Errorf("http hits = %d, want 1", httpHits.Load())
	}
	if dingHits.Load() != 0 {
		t.Errorf("dingtalk hits = %d, want 0（停用渠道不应发送）", dingHits.Load())
	}
}

func TestBuildChannel(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(okHandler(rec))
	defer srv.Close()

	ch, err := BuildChannel(ChannelHTTP, srv.URL, nil)
	if err != nil {
		t.Fatalf("BuildChannel(http) err = %v", err)
	}
	if ch.Name() != "http" {
		t.Errorf("Name = %q, want http", ch.Name())
	}
	if err := ch.Send(context.Background(), testMsg()); err != nil {
		t.Fatalf("Send err = %v", err)
	}

	if _, err := BuildChannel("slack", "https://x", nil); err == nil {
		t.Error("BuildChannel(不支持的渠道) 应返回 error")
	}
}

func TestSupported(t *testing.T) {
	for _, name := range []string{"dingtalk", "feishu", "wecom", "http"} {
		if !Supported(name) {
			t.Errorf("Supported(%q) = false, want true", name)
		}
	}
	if Supported("slack") {
		t.Error("Supported(slack) = true, want false")
	}
}
