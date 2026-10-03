//go:build unix

package mesh

import (
	"runtime"
	"sync"
	"syscall"
	"time"
)

var usage struct {
	sync.Mutex
	lastCPU time.Duration
	lastAt  time.Time
}

// processUsage — CPU процесса (ядер, между вызовами) и память кучи Go, МБ.
func processUsage() (cpu, memMB float64) {
	var ru syscall.Rusage
	cpu = -1
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		total := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
		usage.Lock()
		now := time.Now()
		if !usage.lastAt.IsZero() {
			if dt := now.Sub(usage.lastAt); dt > 0 {
				cpu = float64(total-usage.lastCPU) / float64(dt)
			}
		}
		usage.lastCPU, usage.lastAt = total, now
		usage.Unlock()
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return cpu, float64(ms.Sys) / (1 << 20)
}
