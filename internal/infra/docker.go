package infra

import (
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// docker.go：docker stats 单次快照（容器 CPU/内存占用；docker CLI 不可用或失败时降级空列表）。

// dockerStatsRow docker stats --format '{{json .}}' 输出行。
type dockerStatsRow struct {
	CPUPerc  string `json:"CPUPerc"`
	MemPerc  string `json:"MemPerc"`
	MemUsage string `json:"MemUsage"`
	Name     string `json:"Name"`
	PIDs     string `json:"PIDs"`
}

// collectDocker 采集全部运行中容器的资源占用（~100ms 单次快照）。
func collectDocker(ctx context.Context) []ContainerStats {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{json .}}").Output()
	if err != nil {
		// docker CLI 不可用/失败：降级空列表（单机部署必有 docker，但兼容裸跑）
		return nil
	}
	var items []ContainerStats
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r dockerStatsRow
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			slog.Debug("infra: docker stats parse", "err", err)
			continue
		}
		items = append(items, ContainerStats{
			Name:       r.Name,
			CPUPercent: parsePercent(r.CPUPerc),
			MemPercent: parsePercent(r.MemPerc),
			MemUsedMB:  parseMemUsedMB(r.MemUsage),
			PIDs:       parseInt(r.PIDs),
		})
	}
	return items
}

// parsePercent 解析 "0.08%" → 0.08。
func parsePercent(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	if err != nil {
		return 0
	}
	return v
}

// parseMemUsedMB 解析 "170.8MiB / 15.48GiB" → 170.8。
func parseMemUsedMB(s string) float64 {
	parts := strings.Split(s, "/")
	if len(parts) == 0 {
		return 0
	}
	field := strings.TrimSpace(parts[0])
	for _, suffix := range []string{"MiB", "GiB", "KiB", "MB", "GB", "KB"} {
		if strings.HasSuffix(field, suffix) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(field, suffix), 64)
			if err != nil {
				return 0
			}
			switch suffix {
			case "GiB", "GB":
				return v * 1024
			case "KiB", "KB":
				return v / 1024
			default:
				return v
			}
		}
	}
	return 0
}

func parseInt(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}
