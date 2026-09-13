package debugx

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"time"
)

var started = time.Now()

// snapshot is what /debug/runtime.json returns.
type snapshot struct {
	Process    string  `json:"process"`
	GoVersion  string  `json:"go_version"`
	NumCPU     int     `json:"num_cpu"`
	GOMAXPROCS int     `json:"gomaxprocs"`
	UptimeSec  float64 `json:"uptime_sec"`
	At         float64 `json:"at_unix"`

	Goroutines int `json:"goroutines"`
	Threads    int `json:"threads"`

	HeapLiveBytes uint64  `json:"heap_live_bytes"`
	HeapGoalBytes uint64  `json:"heap_goal_bytes"`
	TotalMemBytes uint64  `json:"total_mem_bytes"`
	GCCycles      uint64  `json:"gc_cycles"`
	LastGCPauseNs uint64  `json:"last_gc_pause_ns"`
	CPUUserSec    float64 `json:"cpu_user_sec"`
	CPUGCSec      float64 `json:"cpu_gc_sec"`
	CPUTotalSec   float64 `json:"cpu_total_sec"`
	CPUIdleSec    float64 `json:"cpu_idle_sec"`
	// CPUProcessSec is user+system CPU time from the kernel, fresh on every
	// read. The runtime/metrics CPU classes above only update at GC cycles,
	// so an idle process reports zero for minutes; "cores busy" is computed
	// from this one.
	CPUProcessSec  float64  `json:"cpu_process_sec"`
	MetricsMissing []string `json:"metrics_missing,omitempty"`
}

// wanted maps runtime/metrics names to what the page calls them. Names differ
// between Go versions; a missing one is reported, never invented.
var wanted = []string{
	"/sched/goroutines:goroutines",
	"/sched/gomaxprocs:threads",
	"/gc/heap/live:bytes",
	"/gc/heap/goal:bytes",
	"/memory/classes/total:bytes",
	"/gc/cycles/total:gc-cycles",
	"/cpu/classes/user:cpu-seconds",
	"/cpu/classes/gc/total:cpu-seconds",
	"/cpu/classes/total:cpu-seconds",
	"/cpu/classes/idle:cpu-seconds",
}

func readSnapshot() snapshot {
	samples := make([]metrics.Sample, len(wanted))
	for i, name := range wanted {
		samples[i].Name = name
	}
	metrics.Read(samples)

	s := snapshot{
		Process:    filepath.Base(os.Args[0]),
		GoVersion:  runtime.Version(),
		NumCPU:     runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		UptimeSec:  time.Since(started).Seconds(),
		At:         float64(time.Now().UnixNano()) / 1e9,
		Goroutines: runtime.NumGoroutine(),
	}
	// ThreadCreateProfile with a nil slice returns how many records there
	// are, which is the number of OS threads the runtime has created.
	s.Threads, _ = runtime.ThreadCreateProfile(nil)

	for _, sample := range samples {
		switch sample.Value.Kind() {
		case metrics.KindUint64:
			v := sample.Value.Uint64()
			switch sample.Name {
			case "/sched/goroutines:goroutines":
				s.Goroutines = int(v)
			case "/sched/gomaxprocs:threads":
				s.GOMAXPROCS = int(v)
			case "/gc/heap/live:bytes":
				s.HeapLiveBytes = v
			case "/gc/heap/goal:bytes":
				s.HeapGoalBytes = v
			case "/memory/classes/total:bytes":
				s.TotalMemBytes = v
			case "/gc/cycles/total:gc-cycles":
				s.GCCycles = v
			}
		case metrics.KindFloat64:
			v := sample.Value.Float64()
			switch sample.Name {
			case "/cpu/classes/user:cpu-seconds":
				s.CPUUserSec = v
			case "/cpu/classes/gc/total:cpu-seconds":
				s.CPUGCSec = v
			case "/cpu/classes/total:cpu-seconds":
				s.CPUTotalSec = v
			case "/cpu/classes/idle:cpu-seconds":
				s.CPUIdleSec = v
			}
		default:
			s.MetricsMissing = append(s.MetricsMissing, sample.Name)
		}
	}

	// The last pause is not in runtime/metrics as a scalar; ReadMemStats
	// stops the world for microseconds, which at one call every two seconds
	// is nothing.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if ms.NumGC > 0 {
		s.LastGCPauseNs = ms.PauseNs[(ms.NumGC+255)%256]
	}
	// Before the first cycle the GC-updated metrics read zero; the allocator's
	// own counters do not.
	if s.HeapLiveBytes == 0 {
		s.HeapLiveBytes = ms.HeapAlloc
	}
	if s.HeapGoalBytes == 0 {
		s.HeapGoalBytes = ms.NextGC
	}
	if s.TotalMemBytes == 0 {
		s.TotalMemBytes = ms.Sys
	}
	s.CPUProcessSec = processCPUSeconds()
	return s
}

func runtimeJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(readSnapshot())
}

// livePage serves the goroutine dump made readable, plus the runtime's own
// numbers, refreshed every two seconds by the process that owns them.
//
// It lives here rather than in the dashboard or an artifact for one reason:
// only a page served by the process can fetch the process's loopback
// endpoints. The Angular app runs on another origin and a published artifact
// is blocked from fetching localhost at all.
//
// What it can and cannot show, said plainly on the page: goroutines, threads,
// heap and GC come straight from runtime/metrics; "cores busy" is an average
// derived from CPU seconds per wall second, because the runtime does not
// expose which P is running what at an instant - that is what the execution
// trace (make trace) is for.
func livePage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(liveHTML))
}

// liveHTML is the whole page, compiled into the binary. Embedded rather than
// fetched so the binary carries it: a diagnostics page that needs a build step
// or a CDN is one that is not there when the process is misbehaving at three
// in the morning. It is a file rather than a string constant so that an editor
// lints the HTML, the CSS and the JavaScript in it.
//
//go:embed live.html
var liveHTML string
