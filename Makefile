# AIOps 开发常用命令
# 中间件（PostgreSQL）用 docker-compose 启动，应用本体 go run / npm run dev。
.PHONY: infra app build test vet tidy web web-build clean

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
