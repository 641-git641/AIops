//go:build !windows && !linux

package infra

import (
	"runtime"
	"time"
)

// host_other.go：未支持平台的兜底（仅 Go runtime 指标）。

func collectHost(lastCPU, lastProc *cpuSample, start time.Time) HostStats {
	var h HostStats
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	h.GoHeapMB = float64(ms.HeapAlloc) / (1 << 20)
	h.GoRoutines = runtime.NumGoroutine()
	h.UptimeSec = int64(time.Since(start).Seconds())
	return h
}
