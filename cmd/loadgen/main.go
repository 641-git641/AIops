// Command loadgen 是告警洪水压测工具（Phase 3 规模化验收）。
// 以限速/并发的 Prometheus webhook 流量打目标实例，模拟真实告警洪峰：
// alertname 按 variety 轮换（同 fingerprint 内容相同 → 自然产生去重重复），
// 输出吞吐、失败数与 P50/P95/P99 延迟，配合 GET /metrics 观察平台内部构成。
//
// -rate 恒为「告警/秒」（事件速率）；-batch>1 时每条请求携带 batch 条事件，
// 请求速率 = rate/batch（模拟真实 Alertmanager 批量推送；汇总按事件折算）。
//
// 回归基准（Phase 3f）：-summary 写机器可读 JSON 汇总；-compare 与基线汇总对比
// 做回归判定（profile 须一致，吞吐/延迟退化非零退出）——make bench / make regress 封装。
//
// 用法示例：
//
//	go run ./cmd/loadgen -baseurl http://localhost:8081 -rate 2000 -duration 30s
//	go run ./cmd/loadgen -baseurl http://localhost:8081 -rate 2000 -duration 30s -batch 20 -variety 400
//	go run ./cmd/loadgen -rate 2000 -duration 30s -concurrency 300 -summary .bench/latest.json
//	go run ./cmd/loadgen -rate 2000 -duration 30s -concurrency 300 -summary .bench/latest.json -compare .bench/baseline.json
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	baseURL     = flag.String("baseurl", "http://localhost:8080", "目标实例（webhook 端点）")
	rate        = flag.Int("rate", 1000, "目标吞吐（告警/秒）")
	duration    = flag.Duration("duration", 30*time.Second, "压测时长")
	concurrency = flag.Int("concurrency", 20, "并发连接数（每个连接内自行限速）")
	variety     = flag.Int("variety", 200, "告警名轮换数（决定去重重复比例：1/variety）")
	severity    = flag.String("severity", "medium", "告警严重度（medium 仅打热路径；critical 会触发 Agent 流水线）")
	batch       = flag.Int("batch", 1, "每条请求携带的事件数（事件速率 = rate，请求速率 = rate/batch）")
	summaryPath = flag.String("summary", "", "压测汇总写出路径（JSON；空则不写，bench/regress 用它留痕）")
	comparePath = flag.String("compare", "", "基线汇总路径（JSON）：与本次结果做回归对比，退化非零退出（先 make regress-baseline）")
	tolThrough  = flag.Float64("tol-throughput", 0.8, "吞吐退化阈值：本次吞吐 < 基线×该值 判定回归")
	tolLatency  = flag.Float64("tol-latency", 1.5, "延迟退化阈值：本次 P50/P99 > 基线×该值 判定回归")
)

func main() {
	flag.Parse()
	if *rate <= 0 || *duration <= 0 || *concurrency <= 0 {
		log.Fatal("rate/duration/concurrency 必须为正数")
	}
	if *batch <= 0 {
		log.Fatal("batch 必须为正数")
	}
	url := *baseURL + "/api/v1/webhook/prometheus"
	// -rate 恒为事件/秒：每 worker 事件速率 = rate/concurrency，请求速率 = 事件速率/batch
	perWorker := *rate / *concurrency
	if perWorker < 1 {
		perWorker = 1
	}
	workerRate := float64(perWorker) / float64(*batch)

	var (
		sent      atomic.Int64
		ok        atomic.Int64
		failed    atomic.Int64
		dials     atomic.Int64 // 实际新建 TCP 连接数（连接复用诊断）
		latMu     sync.Mutex
		latencies []float64 // 毫秒
		errMu     sync.Mutex
		errClass  = map[string]int64{} // 错误类别 → 次数（压测诊断用）
	)
	// 共享 transport：模拟真实监控系统（Prometheus 等）的长连接 keep-alive 行为，
	// 而非每请求新建连接。默认每 worker 独立 client + MaxIdleConnsPerHost=2 会
	// 触发回环连接风暴（高并发下 accept 拒绝/端口耗尽），掩盖服务端真实能力。
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
		},
		MaxIdleConns:        100000,
		MaxIdleConnsPerHost: 100000,
		IdleConnTimeout:     90 * time.Second,
	}
	sharedClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	classifyErr := func(err error) {
		msg := err.Error()
		cls := "other"
		switch {
		case strings.Contains(msg, "Client.Timeout"):
			cls = "timeout"
		case strings.Contains(msg, "Only one usage of each socket address"):
			cls = "socket-exhausted"
		case strings.Contains(msg, "connection reset"):
			cls = "conn-reset"
		case strings.Contains(msg, "EOF"):
			cls = "eof"
		case strings.Contains(msg, "refused"):
			cls = "refused"
		}
		errMu.Lock()
		errClass[cls]++
		errMu.Unlock()
	}
	next := func() int64 { return sent.Add(1) - 1 } // 全局序号（轮换告警名）

	worker := func(done <-chan struct{}) {
		// 请求间隔 = 1/workerRate 秒；workerRate<1（batch 大时）用浮点计算防除零
		ticker := time.NewTicker(time.Duration(float64(time.Second) / workerRate))
		defer ticker.Stop()
		client := sharedClient
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			n := next() // 请求序号
			payload := payloadOf(n)
			start := time.Now()
			resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
			if err != nil {
				failed.Add(1)
				classifyErr(err)
				continue
			}
			// 必须读完 body 再 Close：不读直接 Close 会让 Transport 判定连接被污染而销毁，
			// 导致每请求新建连接（回环连接风暴 → accept 拒绝），连接复用失效。
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 400 {
				failed.Add(1)
			} else {
				ok.Add(1)
			}
			latMu.Lock()
			latencies = append(latencies, float64(time.Since(start).Microseconds())/1000.0)
			latMu.Unlock()
		}
	}

	fmt.Printf("loadgen: %d alerts/s, %s, workers=%d variety=%d severity=%s batch=%d → %s\n",
		*rate, *duration, *concurrency, *variety, *severity, *batch, url)
	startWall := time.Now()
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); worker(done) }()
	}
	time.Sleep(*duration)
	close(done) // 停止 worker 后等待收尾（否则 wg.Wait 永不返回）
	wg.Wait()
	elapsed := time.Since(startWall)
	sentReq, okReq, failedReq := sent.Load(), ok.Load(), failed.Load()
	sentEvents := sentReq * int64(*batch)
	okEvents := okReq * int64(*batch)
	failedEvents := failedReq * int64(*batch)
	dialsN := dials.Load()
	failRate := 0.0
	if sentEvents > 0 {
		failRate = float64(failedEvents) / float64(sentEvents)
	}

	// 汇总
	latMu.Lock()
	all := append([]float64(nil), latencies...)
	latMu.Unlock()
	sort.Float64s(all)
	pct := func(p float64) float64 {
		if len(all) == 0 {
			return 0
		}
		idx := int(p * float64(len(all)))
		if idx >= len(all) {
			idx = len(all) - 1
		}
		return all[idx]
	}
	// 汇总按事件折算（-rate 与失败率阈值的口径都是「事件」；请求数单独报告）
	rps := float64(okEvents) / elapsed.Seconds()
	fmt.Printf("---- summary ----\n")
	fmt.Printf("elapsed    %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("sent       %d events in %d requests (target %d events)\n", sentEvents, sentReq, *rate*int(*duration/time.Second))
	fmt.Printf("dials      %d (%.1f%% of requests)\n", dialsN, 100*float64(dialsN)/float64(sentReq))
	fmt.Printf("ok         %d events (%d requests)\n", okEvents, okReq)
	fmt.Printf("failed     %d events (%d requests)\n", failedEvents, failedReq)
	fmt.Printf("throughput %.0f alerts/s (%.0f req/s)\n", rps, float64(okReq)/elapsed.Seconds())
	if len(all) > 0 {
		fmt.Printf("latency    p50=%.2fms p95=%.2fms p99=%.2fms max=%.2fms (n=%d, 按请求)\n",
			pct(0.50), pct(0.95), pct(0.99), pct(1.0), len(all))
	}
	errMu.Lock()
	for cls, n := range errClass {
		fmt.Printf("errors      %-18s %d\n", cls, n)
	}
	errMu.Unlock()

	// 汇总 JSON（bench/regress 复用）：记录运行 profile + 关键指标，供基线对比/留痕
	sm := summary{
		Run: runProfile{
			Rate:        *rate,
			DurationS:   (*duration).Seconds(),
			Concurrency: *concurrency,
			Variety:     *variety,
			Batch:       *batch,
			Severity:    *severity,
		},
		ElapsedS:   elapsed.Seconds(),
		Sent:       sentEvents,
		OK:         okEvents,
		Failed:     failedEvents,
		FailRate:   failRate,
		Throughput: rps,
		P50:        pct(0.50),
		P95:        pct(0.95),
		P99:        pct(0.99),
		Max:        pct(1.0),
		Dials:      dialsN,
		Timestamp:  time.Now(),
	}
	if *summaryPath != "" && *summaryPath == *comparePath {
		log.Printf("summary 与 compare 同路径 %s：compare 会读到刚写出的本次结果（自己比自己）——请用不同路径", *summaryPath)
	}
	regressed := false
	if *comparePath != "" {
		regressed = compareWithBaseline(*comparePath, sm)
	}
	if *summaryPath != "" && *summaryPath != *comparePath {
		if err := writeJSON(*summaryPath, sm); err != nil {
			log.Printf("summary: 写入 %s 失败: %v", *summaryPath, err)
		}
	}
	// 失败率超 1% 视为压测未达标（硬门槛；按事件口径）
	if failRate > 0.01 {
		fmt.Println("RESULT: FAIL (failure rate > 1%)")
		os.Exit(1)
	}
	if regressed {
		fmt.Println("RESULT: FAIL (baseline regression)")
		os.Exit(1)
	}
	fmt.Println("RESULT: PASS")
}

// runProfile 压测参数（基线对比要求相同 profile 才有意义，参数不一致直接判失败并指引重记）。
type runProfile struct {
	Rate        int     `json:"rate"`
	DurationS   float64 `json:"duration_s"`
	Concurrency int     `json:"concurrency"`
	Variety     int     `json:"variety"`
	Batch       int     `json:"batch"`
	Severity    string  `json:"severity"`
}

// summary 一次压测的机器可读汇总（bench/regress 的留痕与对比数据单元）。
type summary struct {
	Run        runProfile `json:"run"`
	ElapsedS   float64    `json:"elapsed_s"`
	Sent       int64      `json:"sent_events"`
	OK         int64      `json:"ok_events"`
	Failed     int64      `json:"failed_events"`
	FailRate   float64    `json:"failure_rate"`
	Throughput float64    `json:"throughput_alerts_per_s"`
	P50        float64    `json:"p50_ms"`
	P95        float64    `json:"p95_ms"`
	P99        float64    `json:"p99_ms"`
	Max        float64    `json:"max_ms"`
	Dials      int64      `json:"dials"`
	Timestamp  time.Time  `json:"timestamp"`
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// compareWithBaseline 载入基线汇总，按容差判定吞吐/P50/P99 是否回归；回归返回 true。
// profile（rate/duration/concurrency/variety/batch/severity）不一致视为无意义比较，直接判失败。
func compareWithBaseline(path string, sm summary) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("compare: 无法读取基线 %s: %v（先 make regress-baseline 记录基线）\n", path, err)
		return true
	}
	var base summary
	if err := json.Unmarshal(raw, &base); err != nil {
		fmt.Printf("compare: 基线 %s 不是合法汇总 JSON: %v\n", path, err)
		return true
	}
	if base.Run != sm.Run {
		fmt.Printf("compare: 基线参数与本次不一致，比较无意义（请用相同参数 make regress-baseline 覆盖基线）：\n")
		fmt.Printf("         基线 %+v\n", base.Run)
		fmt.Printf("         本次 %+v\n", sm.Run)
		return true
	}
	fmt.Printf("---- regression vs %s ----\n", path)
	type row struct {
		metric string
		unit   string
		base   float64
		now    float64
		ratio  float64
		bad    bool
	}
	rows := []*row{
		{metric: "throughput", unit: "alerts/s", base: base.Throughput, now: sm.Throughput},
		{metric: "p50", unit: "ms", base: base.P50, now: sm.P50},
		{metric: "p99", unit: "ms", base: base.P99, now: sm.P99},
	}
	var bad []string
	for _, r := range rows {
		if r.base > 0 {
			r.ratio = r.now / r.base
			if r.metric == "throughput" {
				r.bad = r.now < r.base*(*tolThrough)
			} else {
				r.bad = r.now > r.base*(*tolLatency)
			}
		}
		// base==0（如压测没发出请求）：throughput 视为退化；延迟无基线无法比较，不算坏
		if r.base == 0 && r.metric == "throughput" {
			r.bad = true
		}
		if r.bad {
			bad = append(bad, r.metric)
		}
	}
	for _, r := range rows {
		v := "ok"
		if r.bad {
			v = "FAIL"
		}
		fmt.Printf("  %-10s %9.2f %-8s → %9.2f %-8s ratio %5.2f  %s\n",
			r.metric, r.base, r.unit, r.now, r.unit, r.ratio, v)
	}
	if len(bad) > 0 {
		fmt.Printf("compare: 回归指标 %s（容差 throughput<%v× 基线 / latency>%v× 基线）\n",
			strings.Join(bad, ", "), *tolThrough, *tolLatency)
		return true
	}
	fmt.Println("compare: 相对基线无回归")
	return false
}

// payloadOf 构造 Prometheus Alertmanager webhook 请求体，模拟真实告警洪峰的构成：
// 每条请求携带 batch 个事件（事件序号 = 请求序号*batch + j，批内 fingerprint 互异），
// 每个 alertname（轮换 variety 个）经历多轮重发——
//   - 首轮（cycle 0）：全新告警 → new
//   - 偶轮（cycle 2/4/...）：内容与首轮完全一致（含 startsAt）→ 同 fingerprint 同 hash → duplicate
//   - 奇轮（cycle 1/3/...）：startsAt 变化（event 快照含时间戳）→ 同 fingerprint 异 hash → update
//
// 这样一次压测同时覆盖去重新增、内容刷新、完全重复三条路径（构成可在 /metrics 观测）。
func payloadOf(reqSeq int64) []byte {
	alerts := make([]any, *batch)
	for j := 0; j < *batch; j++ {
		alerts[j] = alertOf(reqSeq*int64(*batch) + int64(j))
	}
	payload, _ := json.Marshal(map[string]any{
		"status": "firing",
		"alerts": alerts,
	})
	return payload
}

// alertOf 单个告警对象（seq 为全局事件序号）。
func alertOf(seq int64) map[string]any {
	base := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	idx := seq % int64(*variety)
	cycle := seq / int64(*variety)
	start := base
	if cycle%2 == 1 { // 奇轮：内容变化（startsAt 更新）→ update 路径
		start = base.Add(time.Duration(seq) * time.Minute)
	}
	return map[string]any{
		"status": "firing",
		"labels": map[string]any{
			"alertname": fmt.Sprintf("load-%d", idx),
			"severity":  *severity,
			"service":   fmt.Sprintf("svc-%d", idx%8),
		},
		"annotations": map[string]any{"summary": "loadgen benchmark"},
		"startsAt":    start.Format(time.RFC3339),
	}
}
