//go:build linux

package infra

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// host_linux.go：Linux 主机资源采集（/proc/stat + /proc/meminfo + /proc/self/stat，零依赖）。

func collectHost(lastCPU, lastProc *cpuSample, start time.Time) HostStats {
	var h HostStats
	now := time.Now()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	h.GoHeapMB = float64(ms.HeapAlloc) / (1 << 20)
	h.GoRoutines = runtime.NumGoroutine()
	h.UptimeSec = int64(time.Since(start).Seconds())

	// 系统 CPU（/proc/stat 第一行：cpu user nice system idle iowait irq softirq steal）
	if data, err := os.ReadFile("/proc/stat"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 8 && fields[0] == "cpu" {
			var vals [7]uint64
			for i := range vals {
				vals[i], _ = strconv.ParseUint(fields[i+1], 10, 64)
			}
			idle := vals[3] + vals[4] // idle + iowait
			kernel := vals[2] + vals[5] + vals[6]
			user := vals[0] + vals[1]
			if lastCPU.idle > 0 && !lastCPU.wall.IsZero() {
				dIdle := idle - lastCPU.idle
				dTotal := (kernel - lastCPU.kernel) + (user - lastCPU.user)
				if dTotal > 0 {
					h.CPUPercent = float64(dTotal-dIdle) / float64(dTotal) * 100
				}
			}
			lastCPU.idle, lastCPU.kernel, lastCPU.user, lastCPU.wall = idle, kernel, user, now
		}
	}
	// 进程 CPU（/proc/self/stat 第 14/15 字段 utime/stime，单位 clock ticks=100/s）
	if data, err := os.ReadFile("/proc/self/stat"); err == nil {
		// 字段含括号内进程名（可能含空格）——从最后一个 ')' 后取字段
		s := string(data)
		if idx := strings.LastIndex(s, ")"); idx >= 0 {
			rest := strings.Fields(s[idx+1:])
			if len(rest) >= 2 {
				utime, _ := strconv.ParseUint(rest[0], 10, 64)
				stime, _ := strconv.ParseUint(rest[1], 10, 64)
				if lastProc.pKernel > 0 && !lastProc.pWall.IsZero() {
					d := (utime - lastProc.pUser) + (stime - lastProc.pKernel)
					elapsed := now.Sub(lastProc.pWall)
					if elapsed > 0 {
						h.ProcessCPUPercent = float64(d) / (float64(elapsed.Nanoseconds()) / 1e9 / 100) * 100
					}
				}
				lastProc.pUser, lastProc.pKernel, lastProc.pWall = utime, stime, now
			}
		}
	}
	// 内存（/proc/meminfo）
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		var totalKB, availKB uint64
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			switch f[0] {
			case "MemTotal:":
				totalKB, _ = strconv.ParseUint(f[1], 10, 64)
			case "MemAvailable:":
				availKB, _ = strconv.ParseUint(f[1], 10, 64)
			}
		}
		if totalKB > 0 {
			h.MemTotalMB = float64(totalKB) / 1024
			h.MemUsedMB = float64(totalKB-availKB) / 1024
			h.MemPercent = float64(totalKB-availKB) / float64(totalKB) * 100
		}
	}
	return h
}
