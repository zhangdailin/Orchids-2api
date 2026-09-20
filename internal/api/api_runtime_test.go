package api

import (
	"strings"
	"testing"
	"time"
)

// cpuStat builds the `cpu` line of /proc/stat from the fields the parser reads:
// user, nice, system, idle, iowait (all other fields zero).
func cpuStat(user, nice, system, idle, iowait uint64) string {
	fields := []string{"cpu",
		itoa(user), itoa(nice), itoa(system), itoa(idle), itoa(iowait),
		"0", "0", "0", "0", "0"}
	return strings.Join(fields, " ") + "\ncpu0 1 2 3 4 5\n"
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	buf := make([]byte, 0, 20)
	for v > 0 {
		buf = append([]byte{byte('0' + v%10)}, buf...)
		v /= 10
	}
	return string(buf)
}

func resetHostCPU() {
	hostCPU.Lock()
	hostCPU.total, hostCPU.idle = 0, 0
	hostCPU.sampledAt = time.Time{}
	hostCPU.value, hostCPU.window, hostCPU.hasValue = 0, 0, false
	hostCPU.Unlock()
}

func TestSampleHostCPURequiresAWindow(t *testing.T) {
	resetHostCPU()
	start := time.Now()

	// The first reading can only open a window.
	if _, _, ok := sampleHostCPU(cpuStat(100, 0, 100, 800, 0), start); ok {
		t.Fatal("the first sample reported a rate without a baseline")
	}
	// A second reading 10ms later spans at most one tick. On an idle host the
	// request that asks for the metric is itself the only thing that ran, which
	// is exactly how the panel used to show 100%.
	if busy, _, ok := sampleHostCPU(cpuStat(100, 0, 101, 800, 0), start.Add(10*time.Millisecond)); ok {
		t.Fatalf("a sub-tick window reported a rate: %.1f%%", busy*100)
	}
	// Two seconds later the window is real: 200 of 200 ticks idle means 0% busy.
	busy, window, ok := sampleHostCPU(cpuStat(100, 0, 100, 1000, 0), start.Add(2*time.Second))
	if !ok || window != 2*time.Second {
		t.Fatalf("ok=%v window=%s", ok, window)
	}
	if busy != 0 {
		t.Fatalf("busy=%.3f want 0", busy)
	}
	// A sub-tick reading right after a valid one must repeat the last value and
	// window instead of inventing 100%.
	busy, window, ok = sampleHostCPU(cpuStat(100, 0, 101, 1000, 0), start.Add(2*time.Second+10*time.Millisecond))
	if !ok || busy != 0 || window != 2*time.Second {
		t.Fatalf("busy=%.3f window=%s ok=%v want the previous 0%%/2s", busy, window, ok)
	}
	// A later window reports its own rate: 900 ticks elapsed since the previous
	// reading and 600 of them were idle, so 33.3% busy.
	busy, window, ok = sampleHostCPU(cpuStat(200, 0, 300, 1600, 0), start.Add(10*time.Second))
	if !ok {
		t.Fatal("a real window reported no rate")
	}
	if got := busy * 100; got < 33 || got > 34 {
		t.Fatalf("busy=%.2f%% want ~33.3%% (window %s)", got, window)
	}
	if window != 8*time.Second {
		t.Fatalf("window=%s want 8s", window)
	}
}

func TestSampleHostCPUSurvivesCounterReset(t *testing.T) {
	resetHostCPU()
	start := time.Now()
	sampleHostCPU(cpuStat(100, 0, 100, 800, 0), start)
	if _, _, ok := sampleHostCPU(cpuStat(100, 0, 100, 1000, 0), start.Add(3*time.Second)); !ok {
		t.Fatal("a valid window was rejected")
	}
	// Counters moving backwards means the machine rebooted under us: the next
	// reading has to open a new window rather than divide by a negative delta.
	if _, _, ok := sampleHostCPU(cpuStat(1, 0, 1, 10, 0), start.Add(4*time.Second)); ok {
		t.Fatal("a reset counter produced a rate")
	}
	if _, _, ok := sampleHostCPU(cpuStat(1, 0, 1, 12, 0), start.Add(6*time.Second)); !ok {
		t.Fatal("the sampler did not recover after a counter reset")
	}
}

// The metric text has to say which window it measured, so a percentile-looking
// number cannot be read as an instantaneous one.
func TestHostCPUMetricDetailNamesItsWindow(t *testing.T) {
	resetHostCPU()
	start := time.Now()
	sampleHostCPU(cpuStat(100, 0, 100, 800, 0), start)
	busy, window, ok := sampleHostCPU(cpuStat(100, 0, 100, 1000, 0), start.Add(3*time.Second))
	if !ok {
		t.Fatal("no rate")
	}
	metric := runtimeMetric("主机 CPU", "0.0%", "整机所有核心", true, busy*100)
	if metric["status"] != "ok" {
		t.Fatalf("status=%v", metric["status"])
	}
	if window.Seconds() != 3 {
		t.Fatalf("window=%s", window)
	}
}
