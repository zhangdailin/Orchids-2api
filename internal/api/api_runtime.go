package api

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/goccy/go-json"

	"orchids-api/internal/loadbalancer"
)

// CPU is sampled from cumulative Linux scheduler ticks, never since boot and
// never by sleeping in an HTTP handler. Memory is host scope, RSS is process scope.
var hostCPU = struct {
	sync.Mutex
	total, idle uint64
}{}

func parseCPUTicks(raw string) (total, idle uint64, err error) {
	line := strings.SplitN(raw, "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("invalid /proc/stat")
	}
	for i, field := range fields[1:] {
		if i >= 8 {
			break
		}
		v, e := strconv.ParseUint(field, 10, 64)
		if e != nil {
			return 0, 0, e
		}
		total += v
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return
}
func parseHostMemory(raw string) (used, total uint64) {
	values := map[string]uint64{}
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			v, _ := strconv.ParseUint(f[1], 10, 64)
			values[strings.TrimSuffix(f[0], ":")] = v * 1024
		}
	}
	total = values["MemTotal"]
	available, ok := values["MemAvailable"]
	if !ok {
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	if available < total {
		used = total - available
	}
	return
}
func runtimeMetric(label, value, detail string, available bool, percent float64) map[string]interface{} {
	status := "unknown"
	if available {
		status = "ok"
		if percent >= 90 {
			status = "critical"
		} else if percent >= 80 {
			status = "warn"
		}
	}
	return map[string]interface{}{"label": label, "value": value, "detail": detail, "available": available, "status": status}
}
func (a *API) HandleOpsRuntime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cpu := runtimeMetric("主机 CPU", "", "需要 Linux /proc 采集", false, 0)
	if raw, err := os.ReadFile("/proc/stat"); err == nil {
		total, idle, err := parseCPUTicks(string(raw))
		if err == nil {
			hostCPU.Lock()
			if total > hostCPU.total && hostCPU.total > 0 && idle >= hostCPU.idle {
				busy := 1 - float64(idle-hostCPU.idle)/float64(total-hostCPU.total)
				if busy < 0 {
					busy = 0
				}
				cpu = runtimeMetric("主机 CPU", fmt.Sprintf("%.1f%%", busy*100), "整机所有核心 · 两次采样间隔", true, busy*100)
			} else {
				cpu = runtimeMetric("主机 CPU", "", "等待下一次采样", false, 0)
			}
			hostCPU.total = total
			hostCPU.idle = idle
			hostCPU.Unlock()
		}
	}
	memory := runtimeMetric("主机内存", "", "需要 Linux /proc 采集", false, 0)
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		used, total := parseHostMemory(string(raw))
		if total > 0 {
			percent := float64(used) / float64(total) * 100
			memory = runtimeMetric("主机内存", fmt.Sprintf("%.1f%%", percent), formatRuntimeBytes(used)+" / "+formatRuntimeBytes(total)+" · 已用/总量", true, percent)
		}
	}
	rss := runtimeMetric("进程内存 RSS", "", "需要 Linux /proc 采集", false, 0)
	if raw, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) >= 2 {
			pages, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				rss = runtimeMetric("进程内存 RSS", formatRuntimeBytes(pages*uint64(os.Getpagesize())), "当前服务常驻物理内存", true, 0)
			}
		}
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	refreshing := 0
	if a != nil && a.refreshConcurrency != nil {
		refreshing = a.refreshConcurrency()
	}
	metrics := []map[string]interface{}{cpu, memory, rss, runtimeMetric("Go 堆内存", formatRuntimeBytes(mem.Alloc), "当前已分配", true, 0), runtimeMetric("Goroutine", strconv.Itoa(runtime.NumGoroutine()), "当前协程数", true, 0), runtimeMetric("账号刷新", strconv.Itoa(refreshing), "正在刷新", true, 0)}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"available": true, "metrics": metrics})
}
func formatRuntimeBytes(value uint64) string {
	if value >= 1<<30 {
		return fmt.Sprintf("%.2f GB", float64(value)/(1<<30))
	}
	return fmt.Sprintf("%.1f MB", float64(value)/(1<<20))
}

func (a *API) SetConnectionTracker(tracker loadbalancer.ConnTracker) { a.connTracker = tracker }
