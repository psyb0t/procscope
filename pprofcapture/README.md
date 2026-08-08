# pprofcapture — live pprof profiles, returned as a value

Takes a profile of the running process and hands it back as a struct. No listener, no `net/http/pprof`, no port to reach.

## Contents

- [The whole API](#the-whole-api)
- [Step 1: capture something](#step-1-capture-something)
- [Step 2: get it out of the process](#step-2-get-it-out-of-the-process)
- [Step 3: actually read the profile](#step-3-actually-read-the-profile)
- [The eight kinds](#the-eight-kinds)
- [Choosing debug](#choosing-debug)
- [Block and mutex: profiling that arms itself](#block-and-mutex-profiling-that-arms-itself)
- [Every field on Result](#every-field-on-result)
- [Errors](#errors)
- [Recipes](#recipes)

## The whole API

One function, one struct. That's it:

```go
func Capture(kind string, debug int, seconds int) (Result, error)
```

## Step 1: capture something

A complete program. Run it and it prints a heap profile you can open in `go tool pprof`:

```go
package main

import (
	"fmt"
	"os"

	"github.com/psyb0t/procscope/pprofcapture"
)

func main() {
	// kind=heap, debug=0 (protobuf), seconds=0 (not a windowed kind)
	result, err := pprofcapture.Capture(pprofcapture.KindHeap, pprofcapture.DebugProto, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "capture failed:", err)
		os.Exit(1)
	}

	fmt.Println("kind:     ", result.Kind)
	fmt.Println("encoding: ", result.Encoding)
	fmt.Println("captured: ", result.GeneratedAt)
	fmt.Println("note:     ", result.Note)
	fmt.Println()
	fmt.Println(result.Profile) // base64 of a gzipped pprof protobuf
}
```

```bash
go run . > profile.txt
```

## Step 2: get it out of the process

`Result` is a plain struct with `json` tags, so any transport works. A complete HTTP handler:

```go
package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/psyb0t/procscope/pprofcapture"
)

// GET /debug/profile?kind=heap&debug=0&seconds=0
func profileHandler(w http.ResponseWriter, r *http.Request) {
	// Put your own auth in front of this. It exposes memory contents and
	// full goroutine stacks.
	debug, _ := strconv.Atoi(r.URL.Query().Get("debug"))
	seconds, _ := strconv.Atoi(r.URL.Query().Get("seconds"))

	result, err := pprofcapture.Capture(r.URL.Query().Get("kind"), debug, seconds)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(result); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func main() {
	http.HandleFunc("/debug/profile", profileHandler)
	_ = http.ListenAndServe(":8080", nil)
}
```

```bash
curl -s 'localhost:8080/debug/profile?kind=heap' | jq -r .profile > prof.b64
```

## Step 3: actually read the profile

`debug=0` gives you base64 of a **gzipped protobuf**. Decode it, then point the real tooling at it:

```bash
base64 -d < prof.b64 > prof.pb.gz
go tool pprof -http=: prof.pb.gz          # flame graph in a browser
go tool pprof -top prof.pb.gz             # top functions, in the terminal
go tool pprof -inuse_space prof.pb.gz     # retained memory (heap default)
go tool pprof -alloc_space prof.pb.gz     # cumulative allocation churn
```

One-liner from the `curl` above:

```bash
curl -s 'localhost:8080/debug/profile?kind=heap' \
  | jq -r .profile | base64 -d > prof.pb.gz && go tool pprof -http=: prof.pb.gz
```

`debug=1` and `debug=2` give **text instead**, which `go tool pprof` cannot read. Print it, grep it, paste it — don't feed it to the tool.

## The eight kinds

| Constant | `kind` | Blocks? | Answers |
|---|---|---|---|
| `KindHeap` | `heap` | no | What is holding memory *right now* (`inuse_space`). The leak profile. |
| `KindAllocs` | `allocs` | no | What has allocated the most since start. Churn, not retention. |
| `KindGoroutine` | `goroutine` | no | What every goroutine is doing. The leak-finder with `debug=1`. |
| `KindThreadcreate` | `threadcreate` | no | What created OS threads. Rarely what you want. |
| `KindCPU` | `cpu` | **yes** | What burned CPU during the window. |
| `KindTrace` | `trace` | **yes** | Full execution trace for `go tool trace`. |
| `KindBlock` | `block` | optional | Where goroutines blocked. Off by default — see below. |
| `KindMutex` | `mutex` | optional | Where lock contention happened. Off by default — see below. |

An empty `kind` means `heap`. Leading/trailing spaces and capitals are fine: `"  HEAP  "` works.

## Choosing debug

| `debug` | Encoding | Use it for |
|---|---|---|
| `0` (`DebugProto`) | `base64+gzip` | Anything you want to open in `go tool pprof`. The default choice. |
| `1` | `text` | `goroutine`: **groups identical stacks with a count**. This is how you spot a leak — one stack with 40,000 goroutines behind it. |
| `2` (`DebugMax`) | `text` | `goroutine`: every stack in full, ungrouped. Huge. Use when you need the individual goroutine. |

Out-of-range values are clamped rather than rejected: `-5` becomes `0`, `99` becomes `2`. `cpu` and `trace` ignore `debug` entirely — they have no text form, and the returned `Debug` field will read `0` so you can see that happened.

## Block and mutex: profiling that arms itself

Both are **off by default in every Go process**, because leaving them on costs something. Normally that means you cannot profile contention unless you predicted you'd need to — which is a redeploy, at the worst possible moment.

Pass `seconds > 0` and this package enables the profiler, waits, captures, then puts the knob back:

```go
// Enable mutex profiling for 10 seconds, capture, restore. Blocks for 10s.
result, err := pprofcapture.Capture(pprofcapture.KindMutex, pprofcapture.DebugProto, 10)
```

```go
// seconds = 0 does NOT enable anything. You get whatever already accumulated,
// which on a process that never enabled it is an empty profile.
result, err := pprofcapture.Capture(pprofcapture.KindMutex, pprofcapture.DebugProto, 0)
```

Three things worth knowing:

**The window is clamped.** `seconds <= 0` on `cpu`/`trace` becomes `5` (`DefaultSeconds`); anything above `30` (`MaxSeconds`) becomes `30`. A single call cannot hold a request open indefinitely.

**The restore is exact, including when someone else got there first.** `runtime.SetBlockProfileRate` has no getter, so this package tracks whether *it* was the one that set a nonzero rate, and only resets it if so. The mutex fraction is restored to whatever preceded the call, so an already-enabled profile is left alone. Concurrent captures serialise, since both knobs are process-global.

**The buffers never reset.** Block and mutex profiles accumulate across every window since process start — there is no reset API in Go. So a capture shows everything since boot, not just your window. To see what happened *during* an interval, take two captures and diff them:

```bash
go tool pprof -base before.pb.gz after.pb.gz
```

The returned `Note` says this too, so a caller reading only the payload still learns it.

## Every field on Result

```go
type Result struct {
	Kind        string `json:"kind"`         // resolved kind — "heap" if you passed ""
	Debug       int    `json:"debug"`        // resolved debug — clamped, and 0 for cpu/trace
	Seconds     int    `json:"seconds,omitempty"` // resolved window; absent for snapshots
	Encoding    string `json:"encoding"`     // "base64+gzip" or "text"
	Profile     string `json:"profile"`      // the payload
	GeneratedAt string `json:"generated_at"` // RFC3339, UTC
	Note        string `json:"note"`         // how to consume Profile
}
```

Read `Encoding` before doing anything with `Profile` — it is the resolved truth, whereas the `debug` you passed may have been clamped or ignored.

## Errors

Both wrap sentinels from [`common-go/errors`](https://github.com/psyb0t/common-go), so match with `errors.Is`:

```go
result, err := pprofcapture.Capture("nonsense", 0, 0)
if errors.Is(err, commonerrors.ErrInvalidValue) {
	// unknown kind — the message names what you passed
}
```

| Sentinel | When |
|---|---|
| `ErrInvalidValue` | `kind` is not one of the eight |
| `ErrNotFound` | the runtime has no profile registered under that name |

There is deliberately no silent fallback: an unknown kind is an error, not a quiet `heap`.

## Recipes

**Is something leaking goroutines?**

```go
result, _ := pprofcapture.Capture(pprofcapture.KindGoroutine, 1, 0)
fmt.Println(result.Profile) // stacks grouped by count, biggest first
```

**What is holding memory?**

```go
result, _ := pprofcapture.Capture(pprofcapture.KindHeap, pprofcapture.DebugProto, 0)
// -> go tool pprof -inuse_space
```

**What is eating CPU?** (blocks 15s)

```go
result, _ := pprofcapture.Capture(pprofcapture.KindCPU, 0, 15)
// -> go tool pprof -http=:
```

**Is a lock the bottleneck?** (blocks 10s, enables and restores mutex profiling)

```go
result, _ := pprofcapture.Capture(pprofcapture.KindMutex, pprofcapture.DebugProto, 10)
```
