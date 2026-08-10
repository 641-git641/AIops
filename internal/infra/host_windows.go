//go:build windows

package infra

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// host_windows.go：Windows 主机资源采集（kernel32 GetSystemTimes/GlobalMemoryStatusEx/
// GetProcessTimes；x/sys 未封装，直接声明 syscall）。

var (
	kernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes  = kernel32.NewProc("GetSystemTimes")
	procGlobalMemStatus = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetProcessTimes = kernel32.NewProc("GetProcessTimes")
	procDiskFree        = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// memoryStatusEx GlobalMemoryStatusEx 输入输出结构。
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func getSystemTimes() (idle, kernel, user uint64, ok bool) {
	var i, k, u syscall.Filetime
	r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if r == 0 {
		return 0, 0, 0, false
	}
	return ft2u64(i), ft2u64(k), ft2u64(u), true
}

func getProcessTimes() (kernel, user uint64, ok bool) {
	var c, e, k, u syscall.Filetime
	h := uintptr(windows.CurrentProcess())
	r, _, _ := procGetProcessTimes.Call(h, uintptr(unsafe.Pointer(&c)), uintptr(unsafe.Pointer(&e)),
		uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if r == 0 {
		return 0, 0, false
	}
	return ft2u64(k), ft2u64(u), true
}

func ft2u64(ft syscall.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// collectHost 采集主机资源：CPU 需两次采样差（间隔 ~100ms），调用方持有 cpuSample。
func collectHost(lastCPU, lastProc *cpuSample, start time.Time) HostStats {
	var h HostStats
	now := time.Now()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	h.GoHeapMB = float64(ms.HeapAlloc) / (1 << 20)
	h.GoRoutines = runtime.NumGoroutine()
	h.UptimeSec = int64(time.Since(start).Seconds())

	// 系统 CPU
	if idle, kernel, user, ok := getSystemTimes(); ok {
		if lastCPU.idle > 0 && !lastCPU.wall.IsZero() {
			dIdle := idle - lastCPU.idle
			dTotal := (kernel - lastCPU.kernel) + (user - lastCPU.user)
			if dTotal > 0 {
				h.CPUPercent = float64(dTotal-dIdle) / float64(dTotal) * 100
			}
		}
		lastCPU.idle, lastCPU.kernel, lastCPU.user, lastCPU.wall = idle, kernel, user, now
	}
	// 进程 CPU（相对单核 100%）
	if pk, pu, ok := getProcessTimes(); ok {
		if lastProc.pKernel > 0 && !lastProc.pWall.IsZero() {
			d := (pk - lastProc.pKernel) + (pu - lastProc.pUser)
			elapsed := now.Sub(lastProc.pWall)
			if elapsed > 0 {
				h.ProcessCPUPercent = float64(d) / float64(elapsed.Nanoseconds()) * 100
			}
		}
		lastProc.pKernel, lastProc.pUser, lastProc.pWall = pk, pu, now
	}
	// 内存
	var mse memoryStatusEx
	mse.Length = uint32(unsafe.Sizeof(mse))
	if r, _, _ := procGlobalMemStatus.Call(uintptr(unsafe.Pointer(&mse))); r != 0 {
		h.MemTotalMB = float64(mse.TotalPhys) / (1 << 20)
		h.MemUsedMB = float64(mse.TotalPhys-mse.AvailPhys) / (1 << 20)
		if mse.TotalPhys > 0 {
			h.MemPercent = float64(mse.TotalPhys-mse.AvailPhys) / float64(mse.TotalPhys) * 100
		}
	}
	// 工作目录所在盘剩余
	var free, total, totalFree uint64
	if r, _, _ := procDiskFree.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("."))),
		uintptr(unsafe.Pointer(&free)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree))); r != 0 {
		h.DiskFreeGB = float64(free) / (1 << 30)
	}
	return h
}
