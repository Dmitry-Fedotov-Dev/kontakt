//go:build !unix

package mesh

import "runtime"

func processUsage() (cpu, memMB float64) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return -1, float64(ms.Sys) / (1 << 20)
}
