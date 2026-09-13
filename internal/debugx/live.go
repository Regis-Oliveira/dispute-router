package debugx

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"syscall"
	"time"
)

// processCPUSeconds is user plus system time for this process, from the
// kernel. Unix only, which is every machine this runs on.
func processCPUSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 +
		float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

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

// liveHTML is the whole page. Inline so the binary carries it: a diagnostics
// page that needs a build step or a CDN is one that is not there when the
// process is misbehaving at three in the morning.
const liveHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Runtime · live</title>
<style>
  :root { --bg:#0f1519; --card:#161e24; --ink:#e4ebef; --ink2:#a9b6bf; --ink3:#73828c; --rule:#25313a;
          --accent:#5fc3da; --accent2:#0b7a93; --ok:#7fcf9f; --warn:#d9ab5f; --crit:#e08b88; --mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace; }
  * { box-sizing:border-box } body { margin:0; background:var(--bg); color:var(--ink); font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
  .wrap { max-width: 78rem; margin: 0 auto; padding: 1.4rem 1.6rem 4rem; }
  header { display:flex; flex-wrap:wrap; gap:0.6rem 1.6rem; align-items:baseline; border-bottom:1px solid var(--rule); padding-bottom:0.8rem; }
  header h1 { font-size:1.15rem; margin:0; font-weight:600 } header .meta { color:var(--ink3); font-family:var(--mono); font-size:0.78rem }
  header .meta b { color:var(--ink2); font-weight:500 }
  .tiles { display:grid; grid-template-columns: repeat(auto-fit, minmax(11rem,1fr)); gap:0.8rem; margin:1.1rem 0; }
  .tile { background:var(--card); border:1px solid var(--rule); border-radius:6px; padding:0.7rem 0.9rem; }
  .tile .l { font-size:0.68rem; letter-spacing:0.08em; text-transform:uppercase; color:var(--ink3); font-family:var(--mono) }
  .tile .v { font-size:1.55rem; font-weight:600; font-variant-numeric:tabular-nums; line-height:1.2; margin:0.15rem 0 }
  .tile .s { font-size:0.78rem; color:var(--ink2); font-family:var(--mono) }
  .tile svg { display:block; width:100%; height:26px; margin-top:0.35rem }
  h2 { font-size:0.8rem; letter-spacing:0.08em; text-transform:uppercase; color:var(--ink3); margin:1.8rem 0 0.6rem; font-family:var(--mono) }
  .lanes { display:grid; gap:0.35rem } .lane { display:grid; grid-template-columns: 3.2rem 1fr; gap:0.6rem; align-items:center; font-family:var(--mono); font-size:0.78rem; color:var(--ink2) }
  .lane .bar { height:14px; background:var(--card); border:1px solid var(--rule); border-radius:3px; overflow:hidden } .lane .bar i { display:block; height:100%; background:var(--accent2); transition:width .6s ease }
  .note { color:var(--ink3); font-size:0.8rem; margin:0.4rem 0 0 }
  table { width:100%; border-collapse:collapse; font-size:0.85rem } th,td { text-align:left; padding:0.45rem 0.6rem; border-bottom:1px solid var(--rule); vertical-align:top }
  th { color:var(--ink3); font-family:var(--mono); font-size:0.68rem; letter-spacing:0.08em; text-transform:uppercase; font-weight:500 }
  td.n { font-family:var(--mono); font-variant-numeric:tabular-nums; text-align:right; white-space:nowrap } td.mono { font-family:var(--mono); font-size:0.78rem }
  .state { display:inline-block; font-family:var(--mono); font-size:0.68rem; padding:0.05rem 0.4rem; border-radius:3px; background:#1f2a32; color:var(--ink2) }
  .state.running { background:#173a2a; color:var(--ok) } .state.runnable { background:#3a3016; color:var(--warn) } .state.iowait,.state.select,.state.chan { background:#152b33; color:var(--accent) }
  .grew { color:var(--warn) } details summary { cursor:pointer; color:var(--accent) } pre { font:0.72rem/1.4 var(--mono); color:var(--ink2); background:#0b1114; padding:0.6rem; border-radius:4px; overflow-x:auto; margin:0.4rem 0 0 }
  .bystate { display:flex; flex-wrap:wrap; gap:0.4rem; margin:0.4rem 0 0.8rem } .bystate span { font-family:var(--mono); font-size:0.78rem; background:var(--card); border:1px solid var(--rule); padding:0.15rem 0.5rem; border-radius:3px }
  .err { color:var(--crit); font-family:var(--mono); font-size:0.8rem }
</style>
</head>
<body><div class="wrap">
<header><h1>Runtime · live</h1><span class="meta" id="meta">connecting…</span></header>

<div class="tiles">
  <div class="tile"><div class="l">goroutines</div><div class="v" id="t-g">–</div><div class="s" id="s-g"></div><svg id="sp-g" viewBox="0 0 100 26" preserveAspectRatio="none"></svg></div>
  <div class="tile"><div class="l">OS threads</div><div class="v" id="t-th">–</div><div class="s">created by the runtime so far</div></div>
  <div class="tile"><div class="l">cores busy (avg)</div><div class="v" id="t-cpu">–</div><div class="s" id="s-cpu">of GOMAXPROCS</div><svg id="sp-cpu" viewBox="0 0 100 26" preserveAspectRatio="none"></svg></div>
  <div class="tile"><div class="l">live heap</div><div class="v" id="t-heap">–</div><div class="s" id="s-heap"></div><svg id="sp-heap" viewBox="0 0 100 26" preserveAspectRatio="none"></svg></div>
  <div class="tile"><div class="l">GC cycles</div><div class="v" id="t-gc">–</div><div class="s" id="s-gc"></div></div>
</div>

<h2>Logical processors</h2>
<div class="lanes" id="lanes"></div>
<p class="note" id="lanes-idle"></p>
<p class="note">Each lane is one P (GOMAXPROCS). The fill is the process's average CPU use over the last interval spread across lanes — a rate, not a snapshot. Which P ran which goroutine at an instant is not exposed by the runtime; <code>make trace</code> records exactly that.</p>

<h2>Goroutines, grouped by where they are</h2>
<div class="bystate" id="bystate"></div>
<table><thead><tr><th>count</th><th>state</th><th>waiting in</th><th>created by</th><th></th></tr></thead><tbody id="groups"></tbody></table>
<p class="note">Groups with the same state, top frame and creation site are one row. A count that keeps growing between refreshes is a leak; the "created by" column names the <code>go</code> statement to look at.</p>
<p class="err" id="err"></p>
</div>
<script>
const hist = { g: [], cpu: [], heap: [] }; let prev = null; let prevCounts = {};
const $ = id => document.getElementById(id);
const fmtB = b => b >= 1<<30 ? (b/(1<<30)).toFixed(2)+" GiB" : b >= 1<<20 ? (b/(1<<20)).toFixed(1)+" MiB" : (b/1024).toFixed(0)+" KiB";
function spark(id, arr, max) {
  const svg = $(id); if (!arr.length) return;
  const m = max ?? Math.max(1, ...arr); const n = Math.max(arr.length, 30);
  const pts = arr.map((v,i) => ((i + (n-arr.length))/(n-1)*100).toFixed(1) + "," + (24 - (v/m)*22).toFixed(1)).join(" ");
  svg.innerHTML = '<polyline fill="none" stroke="#5fc3da" stroke-width="1.5" points="' + pts + '"/>';
}
function push(a, v) { a.push(v); if (a.length > 60) a.shift(); }

async function tick() {
  try {
    const [rt, dump] = await Promise.all([
      fetch("/debug/runtime.json", {cache:"no-store"}).then(r => r.json()),
      fetch("/debug/pprof/goroutine?debug=2", {cache:"no-store"}).then(r => r.text()),
    ]);
    $("err").textContent = "";
    $("meta").innerHTML = "<b>" + rt.process + "</b> · " + rt.go_version + " · " + rt.num_cpu + " CPUs · GOMAXPROCS " + rt.gomaxprocs + " · up " + Math.round(rt.uptime_sec) + "s";

    push(hist.g, rt.goroutines); $("t-g").textContent = rt.goroutines; spark("sp-g", hist.g);
    $("t-th").textContent = rt.threads;

    let busy = null;
    if (prev) {
      const wall = rt.at_unix - prev.at_unix;
      const cpu = rt.cpu_process_sec - prev.cpu_process_sec;
      if (wall > 0 && cpu >= 0) busy = cpu / wall;
    }
    if (busy !== null) {
      push(hist.cpu, busy); $("t-cpu").textContent = busy.toFixed(2); spark("sp-cpu", hist.cpu, rt.gomaxprocs);
      // The GC classes only advance at GC cycles, so the share is measured
      // against the kernel's CPU time for the same interval and clamped: a
      // cycle that lands in one interval can read above 100% otherwise.
      const wall = rt.at_unix - prev.at_unix;
      const gc = Math.max(0, rt.cpu_gc_sec - prev.cpu_gc_sec), proc = Math.max(1e-9, rt.cpu_process_sec - prev.cpu_process_sec);
      const share = Math.min(100, Math.round(100 * gc / proc));
      $("s-cpu").textContent = "of " + rt.gomaxprocs + " · " + (busy*100).toFixed(1) + "% of one core · GC " + share + "%";
    }

    push(hist.heap, rt.heap_live_bytes); $("t-heap").textContent = fmtB(rt.heap_live_bytes); spark("sp-heap", hist.heap, Math.max(rt.heap_goal_bytes, ...hist.heap));
    $("s-heap").textContent = "next GC at " + fmtB(rt.heap_goal_bytes) + " · RSS-ish " + fmtB(rt.total_mem_bytes);
    $("t-gc").textContent = rt.gc_cycles; $("s-gc").textContent = "last pause " + (rt.last_gc_pause_ns/1000).toFixed(0) + " µs";

    const lanes = $("lanes"); const P = rt.gomaxprocs;
    if (lanes.children.length !== P) lanes.innerHTML = Array.from({length:P}, (_,i) => '<div class="lane"><span>P' + i + '</span><div class="bar"><i style="width:0%"></i></div></div>').join("");
    for (let i = 0; i < P; i++) { const f = busy === null ? 0 : Math.max(0, Math.min(1, busy - i)); lanes.children[i].querySelector("i").style.width = (f*100).toFixed(1) + "%"; }
    $("lanes-idle").textContent = busy !== null && busy < 0.005 ? "Idle over the last interval: no goroutine needed a processor. The disputes make emit sends are due in days, and the worker claims only what is due within its lookahead; make emit-rush sends ones due in forty seconds." : "";

    renderGroups(dump);
    prev = rt;
  } catch (e) { $("err").textContent = "refresh failed: " + e; }
}

function parseDump(text) {
  return text.split(/\n\n+/).filter(b => b.startsWith("goroutine ")).map(block => {
    const lines = block.split("\n");
    const m = /^goroutine (\d+) \[([^\]]+)\]/.exec(lines[0]);
    const state = m ? m[2].split(",")[0].trim() : "?";
    const frames = []; let createdBy = "";
    for (let i = 1; i < lines.length; i++) {
      const l = lines[i];
      if (l.startsWith("created by ")) { createdBy = l.replace("created by ", "").replace(/ in goroutine \d+$/, ""); break; }
      if (!l.startsWith("\t") && l.trim()) frames.push(l.replace(/\([^()]*\)$/, "").replace(/\(\.\.\.\)$/, ""));
    }
    // The frame worth showing is the first one that is not runtime or stdlib plumbing.
    const own = frames.find(f => f.includes("dispute-router/")) || frames.find(f => !/^(runtime|internal\/poll|sync|net\.|net\/http|os\/signal|golang\.org)/.test(f)) || frames[0] || "";
    return { id: m ? m[1] : "?", state, top: own.replace("github.com/regisoliveira/dispute-router/", ""), createdBy: createdBy.replace("github.com/regisoliveira/dispute-router/", ""), raw: block };
  });
}

function renderGroups(text) {
  const gs = parseDump(text);
  const byState = {}; gs.forEach(g => byState[g.state] = (byState[g.state]||0) + 1);
  $("bystate").innerHTML = Object.entries(byState).sort((a,b) => b[1]-a[1]).map(([s,n]) => "<span>" + n + " " + s + "</span>").join("");
  const groups = {};
  gs.forEach(g => { const k = g.state + "|" + g.top + "|" + g.createdBy; (groups[k] ||= { ...g, count: 0, raws: [] }); groups[k].count++; if (groups[k].raws.length < 3) groups[k].raws.push(g.raw); });
  const rows = Object.entries(groups).sort((a,b) => b[1].count - a[1].count);
  const counts = {};
  $("groups").innerHTML = rows.map(([k, g]) => {
    counts[k] = g.count; const grew = prevCounts[k] !== undefined && g.count > prevCounts[k];
    const cls = g.state.replace(/\s+/g, "").toLowerCase().replace("iowait","iowait");
    return "<tr><td class=\"n" + (grew ? " grew" : "") + "\">" + g.count + (grew ? " ↑" : "") + "</td><td><span class=\"state " + cls + "\">" + g.state + "</span></td><td class=\"mono\">" + esc(g.top) + "</td><td class=\"mono\">" + esc(g.createdBy || "main") + "</td><td><details><summary>stack</summary><pre>" + esc(g.raws.join("\n\n")) + "</pre></details></td></tr>";
  }).join("");
  prevCounts = counts;
}
function esc(s) { return String(s).replace(/&/g,"&amp;").replace(/</g,"&lt;"); }
tick(); setInterval(tick, 2000);
</script>
</body></html>`
