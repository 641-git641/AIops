# AIOps 开发常用命令
# 中间件（PostgreSQL）用 docker-compose 启动，应用本体 go run / npm run dev。
.PHONY: infra app build test vet tidy web web-build clean bench regress regress-baseline bench-batch regress-batch regress-baseline-batch

infra:
	docker compose -f deploy/docker-compose.yml up -d postgres

app:
	AIOPS_DBDSN="host=127.0.0.1 user=aiops password=aiops dbname=aiops port=5432 sslmode=disable TimeZone=Asia/Shanghai" \
		go run ./cmd/aiops

build:
	go build -o bin/aiops ./cmd/aiops

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

web:
	cd web && npm run dev

web-build:
	cd web && npm run build

clean:
	rm -rf bin

# loadgen 回归基准（Phase 3f，依赖 :8080 已运行 — make app / bin/aiops）：
#   make bench            → 跑一次压测，汇总存 .bench/latest.json（不判定）
#   make regress          → 压测 + 与 .bench/baseline.json 对比，吞吐/P50/P99 退化非零退出
#   make regress-baseline → 用当前压测结果覆盖基线（换环境/改参数后需重记）
# 可覆盖：LOADGEN_BASEURL / BENCH_FLAGS（如 "-rate 1000 -duration 10s"）。
LOADGEN_BASEURL ?= http://127.0.0.1:8080
BENCH_FLAGS ?= -rate 2000 -duration 30s -concurrency 300 -variety 200
BENCH_DIR ?= .bench

bench:
	mkdir -p $(BENCH_DIR)
	go run ./cmd/loadgen -baseurl $(LOADGEN_BASEURL) $(BENCH_FLAGS) -summary $(BENCH_DIR)/latest.json

regress:
	mkdir -p $(BENCH_DIR)
	go run ./cmd/loadgen -baseurl $(LOADGEN_BASEURL) $(BENCH_FLAGS) -summary $(BENCH_DIR)/latest.json -compare $(BENCH_DIR)/baseline.json

regress-baseline:
	mkdir -p $(BENCH_DIR)
	go run ./cmd/loadgen -baseurl $(LOADGEN_BASEURL) $(BENCH_FLAGS) -summary $(BENCH_DIR)/baseline.json

# 批量路径回归（Phase 3g 服务端 SaveAlerts 批量写；同三目标语义，-batch 一次带多条）：
#   make regress-baseline-batch → 用当前压测结果覆盖批量基线（换环境/改参数后需重记）
#   make regress-batch          → 压测 + 对比批量基线
# 可覆盖：BENCH_BATCH（单请求事件数）。
BENCH_BATCH ?= 20

bench-batch:
	mkdir -p $(BENCH_DIR)
	go run ./cmd/loadgen -baseurl $(LOADGEN_BASEURL) -rate 2000 -duration 30s -concurrency 300 -variety 200 -batch $(BENCH_BATCH) -summary $(BENCH_DIR)/latest-batch.json

regress-batch:
	mkdir -p $(BENCH_DIR)
	go run ./cmd/loadgen -baseurl $(LOADGEN_BASEURL) -rate 2000 -duration 30s -concurrency 300 -variety 200 -batch $(BENCH_BATCH) -summary $(BENCH_DIR)/latest-batch.json -compare $(BENCH_DIR)/baseline-batch.json

regress-baseline-batch:
	mkdir -p $(BENCH_DIR)
	go run ./cmd/loadgen -baseurl $(LOADGEN_BASEURL) -rate 2000 -duration 30s -concurrency 300 -variety 200 -batch $(BENCH_BATCH) -summary $(BENCH_DIR)/baseline-batch.json
