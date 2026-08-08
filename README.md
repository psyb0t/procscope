# procscope

[![Go Reference](https://pkg.go.dev/badge/github.com/psyb0t/procscope.svg)](https://pkg.go.dev/github.com/psyb0t/procscope)
[![CI](https://github.com/psyb0t/procscope/actions/workflows/pipeline.yml/badge.svg?branch=main)](https://github.com/psyb0t/procscope/actions/workflows/pipeline.yml)
[![coverage](https://raw.githubusercontent.com/psyb0t/procscope/badges/coverage.svg)](https://github.com/psyb0t/procscope/actions/workflows/pipeline.yml)
[![version](https://raw.githubusercontent.com/psyb0t/procscope/badges/version.svg)](https://github.com/psyb0t/procscope/tags)
[![license](https://raw.githubusercontent.com/psyb0t/procscope/badges/license.svg)](LICENSE)
[![imported by](https://raw.githubusercontent.com/psyb0t/procscope/badges/importers.svg)](https://github.com/psyb0t/procscope/blob/badges/importers.md)

Go library that lets a process profile itself — what it's burning CPU on, what's holding memory, what every goroutine is doing — and returns the answer as a **value** instead of serving it on a port.

**Status:** active, early.

## Contents

- [Why Not Just /debug/pprof](#why-not-just-debugpprof)
- [Install](#install)
- [Quick Start](#quick-start)
- [Serving It Over HTTP](#serving-it-over-http)
- [Reading The Profile](#reading-the-profile)
- [The Eight Kinds](#the-eight-kinds)
- [Format And Detail](#format-and-detail)
- [Block And Mutex: Profiling That Arms Itself](#block-and-mutex-profiling-that-arms-itself)
- [Every Field](#every-field)
- [Errors](#errors)
- [Recipes](#recipes)
- [Searching The Process's Own Logs](#searching-the-processs-own-logs)
- [What This Costs At Rest](#what-this-costs-at-rest)
- [Dev Workflow](#dev-workflow)
- [Contributing](#contributing)
- [License](#license)

## Why Not Just /debug/pprof

`net/http/pprof` needs a listener, and the moment you actually want it — a container behind a load balancer, mid-incident, no route to that port — the listener is the one thing you can't reach.

procscope returns profiles as plain structs, so whatever channel you *already* trust to reach the process can serve them: an authenticated admin route, an MCP tool, a CLI subcommand, an RPC.

It composes rather than replaces. Keep shipping profiles somewhere durable; this is the fast path when you need an answer out of *this* process, *now*.

## Install

```bash
go get github.com/psyb0t/procscope
```

## Quick Start

A complete program:

```go
package main

import (
	"fmt"

	"github.com/psyb0t/procscope"
)

func main() {
	// The zero Options is a heap profile in protobuf form — the common case.
	result, err := procscope.Capture(procscope.Options{})
	if err != nil {
		panic(err)
	}

	fmt.Println(result.Kind)     // heap
	fmt.Println(result.Encoding) // base64+gzip
	fmt.Println(result.Profile)  // base64 of a gzipped pprof protobuf
}
```

Everything is a named type, so a typo is a compile error rather than a silent miss:

```go
result, err := procscope.Capture(procscope.Options{
	Kind:   procscope.KindGoroutine,
	Format: procscope.FormatText,
	Detail: procscope.DetailGrouped,
	Window: 10 * time.Second,
})
```

## Serving It Over HTTP

`Result` has `json` tags, so any transport works. A complete handler:

```go
package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/psyb0t/procscope"
)

// GET /debug/profile?kind=heap&format=text&detail=full&window=10s
func profileHandler(w http.ResponseWriter, r *http.Request) {
	// Put your own auth in front of this. It exposes memory contents and
	// full goroutine stacks.
	query := r.URL.Query()

	opts := procscope.Options{
		Kind:   procscope.Kind(query.Get("kind")),
		Format: procscope.Format(query.Get("format")),
		Detail: procscope.Detail(query.Get("detail")),
	}

	if raw := query.Get("window"); raw != "" {
		window, err := time.ParseDuration(raw)
		if err != nil {
			http.Error(w, "bad window: "+raw, http.StatusBadRequest)

			return
		}

		opts.Window = window
	}

	result, err := procscope.Capture(opts)
	if err != nil {
		// Capture rejects an unknown kind/format/detail rather than guessing,
		// so this is a real 400 and the message names the bad value.
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func main() {
	http.HandleFunc("/debug/profile", profileHandler)
	_ = http.ListenAndServe(":8080", nil)
}
```

Empty query params are fine — they mean "use the default", which is exactly what the zero value already does.

## Reading The Profile

`FormatProto` gives base64 of a **gzipped protobuf**. Decode it, then point the real tooling at it:

```bash
curl -s 'localhost:8080/debug/profile?kind=heap' \
  | jq -r .profile | base64 -d > prof.pb.gz

go tool pprof -http=: prof.pb.gz          # flame graph in a browser
go tool pprof -top prof.pb.gz             # top functions, in the terminal
go tool pprof -inuse_space prof.pb.gz     # retained memory (heap default)
go tool pprof -alloc_space prof.pb.gz     # cumulative allocation churn
```

`FormatText` gives readable text instead, which `go tool pprof` **cannot** read. Print it, grep it, paste it — don't feed it to the tool. The returned `Encoding` and `Note` say so, because handing a caller a payload it will misuse is how you get confidently wrong answers.

## The Eight Kinds

| Constant | Blocks? | Answers |
|---|---|---|
| `KindHeap` | no | What is holding memory *right now* (`inuse_space`). The leak profile. |
| `KindAllocs` | no | What has allocated most since start. Churn, not retention. |
| `KindGoroutine` | no | What every goroutine is doing. The leak-finder with `FormatText` + `DetailGrouped`. |
| `KindThreadCreate` | no | What created OS threads. Rarely what you want. |
| `KindCPU` | **yes** | What burned CPU during the window. |
| `KindTrace` | **yes** | Full execution trace for `go tool trace`. |
| `KindBlock` | optional | Where goroutines blocked. Off by default — see below. |
| `KindMutex` | optional | Where lock contention happened. Off by default — see below. |

An empty `Kind` means `KindHeap`. An unknown one is an error, not a quiet default.

## Format And Detail

These are two separate axes, and that is deliberate. `runtime/pprof` takes a single `debug int` where `0` means protobuf and anything higher means text *and* picks a verbosity — two unrelated decisions in one number. procscope splits them and keeps that integer out of its API entirely.

| Field | Values | Effect |
|---|---|---|
| `Format` | `FormatProto` (default) | Gzipped protobuf, base64-encoded. What `go tool pprof` reads. |
| | `FormatText` | Human-readable. Not consumable by `go tool pprof`. |
| `Detail` | `DetailGrouped` (default) | Collapses identical stacks with a count. **This is what finds a goroutine leak** — one stack with 40,000 behind it. |
| | `DetailFull` | Every stack, ungrouped. Huge. Use when you need one specific goroutine. |

`Detail` only applies to `FormatText`; `FormatProto` has one representation, and `Result.Detail` comes back empty for it rather than echoing a value that had no effect.

`KindCPU` and `KindTrace` have no text form at all, so asking for one is an error rather than a `Result` whose `Format` contradicts its `Encoding`.

## Block And Mutex: Profiling That Arms Itself

Both are **off by default in every Go process**, because leaving them on costs something. Normally that means you cannot profile contention unless you predicted you'd need to — which is a redeploy, at the worst possible moment.

Set a `Window` and procscope enables the profiler, waits, captures, then puts the knob back:

```go
// Enable mutex profiling for 10s, capture, restore. Blocks for 10s.
result, err := procscope.Capture(procscope.Options{
	Kind:   procscope.KindMutex,
	Window: 10 * time.Second,
})
```

```go
// No Window does NOT enable anything. You get whatever already accumulated,
// which on a process that never enabled it is an empty profile.
result, err := procscope.Capture(procscope.Options{Kind: procscope.KindMutex})
```

Three things worth knowing:

**The window is clamped.** `<= 0` becomes `DefaultWindow` (5s); anything above `MaxWindow` (30s) is clamped down. A single call cannot hold a request open indefinitely.

**The restore is exact, including when someone else got there first.** `runtime.SetBlockProfileRate` has no getter, so this package tracks whether *it* set a nonzero rate and only resets it if so. The mutex fraction is restored to whatever preceded the call, leaving an already-enabled profile alone. Concurrent captures serialise, since both knobs are process-global.

**The buffers never reset.** Block and mutex profiles accumulate across every window since process start — Go has no reset API. A capture shows everything since boot, not just your window. To see what happened *during* an interval, diff two captures:

```bash
go tool pprof -base before.pb.gz after.pb.gz
```

`Result.Note` says this too, so a caller reading only the payload still learns it.

## Every Field

```go
type Options struct {
	Kind   Kind          // default KindHeap
	Format Format        // default FormatProto
	Detail Detail        // default DetailGrouped; ignored by FormatProto
	Window time.Duration // cpu/trace: how long to sample
	                     // block/mutex: how long to ENABLE, then restore
	                     // snapshots: ignored
}

type Result struct {
	Kind        Kind          `json:"kind"`         // resolved
	Format      Format        `json:"format"`       // resolved
	Detail      Detail        `json:"detail,omitempty"`  // absent for proto
	Window      time.Duration `json:"window,omitempty"`  // absent for snapshots
	Encoding    Encoding      `json:"encoding"`     // base64+gzip | text
	Profile     string        `json:"profile"`
	GeneratedAt string        `json:"generated_at"` // RFC3339, UTC
	Note        string        `json:"note"`         // how to consume Profile
}
```

Read `Encoding` before doing anything with `Profile` — it is the resolved truth, whereas the `Format` you passed may have been defaulted.

## Errors

Both wrap sentinels from [common-go](https://github.com/psyb0t/common-go), so match with `errors.Is`:

```go
_, err := procscope.Capture(procscope.Options{Kind: "nonsense"})
if errors.Is(err, commonerrors.ErrInvalidValue) {
	// the message names what you passed
}
```

| Sentinel | When |
|---|---|
| `ErrInvalidValue` | unknown `Kind`, `Format` or `Detail`, or text asked of `cpu`/`trace` |
| `ErrNotFound` | the runtime has no profile registered under that name |

There is deliberately no silent fallback: an unknown value is an error, not a quiet `heap`.

## Recipes

**Is something leaking goroutines?**

```go
result, _ := procscope.Capture(procscope.Options{
	Kind:   procscope.KindGoroutine,
	Format: procscope.FormatText,
	Detail: procscope.DetailGrouped, // stacks grouped by count, biggest first
})
fmt.Println(result.Profile)
```

**What is holding memory?**

```go
result, _ := procscope.Capture(procscope.Options{Kind: procscope.KindHeap})
// -> go tool pprof -inuse_space
```

**What is eating CPU?** (blocks 15s)

```go
result, _ := procscope.Capture(procscope.Options{
	Kind:   procscope.KindCPU,
	Window: 15 * time.Second,
})
```

**Is a lock the bottleneck?** (blocks 10s, enables and restores mutex profiling)

```go
result, _ := procscope.Capture(procscope.Options{
	Kind:   procscope.KindMutex,
	Window: 10 * time.Second,
})
```

## Searching The Process's Own Logs

procscope does not do this, and deliberately so — [slog-configurator](https://github.com/psyb0t/slog-configurator) already does, on the ring handler itself:

```go
ring := logring.New(logring.Options{})
slogconfigurator.AddHandler(ring)   // the ring IS the slog.Handler

page := ring.Search(logring.SearchOptions{
    Attrs:    map[string]string{"request_id": "abc123"},
    MinLevel: slog.LevelWarn,
    Limit:    50,
})

fmt.Printf("showing %d of %d\n", len(page.Entries), page.Total)
```

`Search`, `Count`, `Tail`, `Clear` and `Stats` are all methods on that handler, so there is nothing for procscope to wrap. A `logsearch` package shipped in v0.1.0 doing exactly that; it was removed in v0.2.0 — see [CHANGELOG.md](CHANGELOG.md).

## What This Costs At Rest

Nothing is enabled until you call it. No listener, no background goroutine, no sampler. Snapshot kinds read state the runtime already maintains, and block and mutex profiling stay off until a window asks for them and go back off afterwards.

## Dev Workflow

```bash
make test           # all tests, -race
make test-coverage  # coverage gate (fails under 90%)
make lint           # go fix + golangci-lint
make lint-fix       # same, with --fix
```

See `make help` for the full list.

## Contributing

Got an idea? Throw in a PR. Found a bug? Raise an issue.

## License

MIT. See [LICENSE](LICENSE).

See [CHANGELOG.md](CHANGELOG.md) for release notes.
