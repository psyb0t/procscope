# procscope

[![Go Reference](https://pkg.go.dev/badge/github.com/psyb0t/procscope.svg)](https://pkg.go.dev/github.com/psyb0t/procscope)
[![CI](https://github.com/psyb0t/procscope/actions/workflows/pipeline.yml/badge.svg?branch=main)](https://github.com/psyb0t/procscope/actions/workflows/pipeline.yml)
[![coverage](https://raw.githubusercontent.com/psyb0t/procscope/badges/coverage.svg)](https://github.com/psyb0t/procscope/actions/workflows/pipeline.yml)
[![version](https://raw.githubusercontent.com/psyb0t/procscope/badges/version.svg)](https://github.com/psyb0t/procscope/tags)
[![license](https://raw.githubusercontent.com/psyb0t/procscope/badges/license.svg)](LICENSE)

Go library that lets a process answer questions about itself — what it's burning CPU on, what's holding memory, what it just logged — and returns the answer as a value instead of serving it on a port.

**Status:** active, early. `pprofcapture` and `logsearch` are stable; more surfaces are planned (see [CHANGELOG.md](CHANGELOG.md)).

## Contents

- [Why Not Just /debug/pprof](#why-not-just-debugpprof)
- [Quick Start](#quick-start)
- [Package Layout](#package-layout)
- [Capturing A Profile](#capturing-a-profile)
- [Searching The Process's Own Logs](#searching-the-processs-own-logs)
- [Building Agent Tooling On Top](#building-agent-tooling-on-top)
- [What This Costs At Rest](#what-this-costs-at-rest)
- [Dev Workflow](#dev-workflow)
- [Contributing](#contributing)
- [License](#license)

## Why Not Just /debug/pprof

`net/http/pprof` needs a listener, and the moment you actually want it — a container behind a load balancer, mid-incident, no route to that port — the listener is the one thing you can't reach.

procscope returns profiles and log matches as plain structs, so whatever channel you *already* trust to reach the process can serve them: an authenticated admin route, an MCP tool, a CLI subcommand, an RPC. No new port, no new attack surface.

It composes rather than replaces. Keep shipping logs and metrics somewhere durable; this is the fast path when you need an answer out of *this* process, *now*.

## Quick Start

```bash
go get github.com/psyb0t/procscope
```

```go
import "github.com/psyb0t/procscope/pprofcapture"

// Retained memory — which types and callsites are holding it.
result, err := pprofcapture.Capture(pprofcapture.KindHeap, pprofcapture.DebugProto, 0)
```

`result.Profile` is base64 of a gzipped pprof protobuf, ready for the real tooling:

```bash
echo "$PROFILE" | base64 -d > prof.pb.gz && go tool pprof -http=: prof.pb.gz
```

## Package Layout

```
pprofcapture/  — live pprof profiles (8 kinds) returned as a value
logsearch/     — search the process's own in-memory log ring
```

Dependencies are deliberately short: `common-go` for error sentinels, `ctxerrors` for wrapping, `slog-configurator` for the log ring. No Prometheus client, no MCP SDK, no `google/pprof` — whatever you build on top picks those.

## Capturing A Profile

Eight kinds. `heap`, `allocs`, `goroutine` and `threadcreate` are instant snapshots; `cpu` and `trace` block for the window; `block` and `mutex` are covered below.

`debug` picks the payload. `DebugProto` (0) gives the protobuf; `1` and `2` give text. For `goroutine`, `debug=2` is full stacks and `debug=1` groups identical stacks by count — which is how you spot a goroutine leak. Text is **not** consumable by `go tool pprof`, and the returned `Encoding` and `Note` say so, because handing a caller a payload it will misuse is how you get confidently wrong answers.

An empty kind means `heap`. An unknown kind returns `commonerrors.ErrInvalidValue` rather than silently defaulting.

### Block and mutex profiling, on demand

Both are off by default in every Go process because leaving them on costs something. That normally means you can't profile contention unless you predicted you'd need to — a redeploy, exactly when you can least afford one.

Pass `seconds > 0` and procscope enables the profiler, waits the window, captures, then puts the knob back as it found it:

```go
result, err := pprofcapture.Capture(pprofcapture.KindMutex, pprofcapture.DebugProto, 10)
```

With `seconds = 0` you get the snapshot path, which does **not** enable anything — usually an empty profile, which is the honest answer rather than a silent enable.

The restore is careful on purpose: `runtime.SetBlockProfileRate` has no getter, so a capture resets the rate only if it was the one that set it nonzero, and the mutex fraction is restored to whatever preceded the call so an already-enabled profile is left alone. Concurrent captures serialise, since both knobs are process-global.

One caveat the `Note` also carries: block and mutex buffers have no reset API, so they accumulate across every window since process start. Diff two captures to see what happened between them.

## Searching The Process's Own Logs

Stack the ring from [slog-configurator](https://github.com/psyb0t/slog-configurator) onto your handler, then read it back:

```go
ring := logring.New(logring.Options{})
slogconfigurator.AddHandler(ring)

result, err := logsearch.Search(ring, logsearch.Options{
    Attrs:    map[string]string{"request_id": "abc123"},
    MinLevel: slog.LevelWarn,
    Limit:    50,
})
```

`Attrs` matches **structured attributes captured off the record**, not a substring of the formatted line. So it behaves the same whether the ring stores JSON or text, and it finds attributes bound far upstream with `logger.With(...)` that never appear on the record itself. Grouped attributes use dotted keys — `WithGroup("http")` logging `status` matches `http.status`.

What this layer adds over calling the ring directly:

- **`TotalMatches` is the count before paging.** Without it you can't tell a full page from the last page, and paging becomes guesswork.
- **A page size it won't exceed** (100 default, 1000 max). Entries are whole log lines, so an unbounded read of "the logs" is a huge payload.
- **Per-line truncation at 4000 runes**, cut on a rune boundary so a multi-byte character never becomes invalid UTF-8. One dumped payload must not crowd out every other match.

A nil ring returns `commonerrors.ErrUnavailable`, not an empty result — "never wired" and "nothing matched" are different answers, and collapsing them sends you hunting for a bug in your filter.

**Scope, stated plainly:** the ring is per process, bounded, and dies with the process. A crashed or restarted replica shows nothing, and in a multi-replica deployment you get whichever replica served the call. It's a debugging aid, not a log store.

## Building Agent Tooling On Top

Every result is a plain struct with `json` tags already shaped for a tool payload — which is the whole reason there's no MCP dependency here. You wire the transport; procscope supplies the answer.

The part worth getting right if you do: **write the tool descriptions so a model can route between them.** Handed several equivalent-looking debug tools, a model picks badly. Say which one is the dashboard and which is the microscope, say what blocks and for how long, and say what each tool *cannot* see — a per-process ring that dies with the process has to advertise that, or the model will confidently report "no errors" for a replica it never looked at.

## What This Costs At Rest

Nothing is enabled until you call it. No listener, no background goroutine, no sampler. Snapshot kinds read state the runtime already maintains; block and mutex stay off until a window asks for them and go back off afterwards; the log ring costs whatever bound you gave it.

## Dev Workflow

```bash
make test           # all tests, -race
make test-coverage  # coverage gate (fails under 90%)
make lint           # go fix + golangci-lint
make lint-fix       # same, with --fix
```

See `make help` for the full list. Concurrency is tested under `-race`, and the profiling knobs are asserted restored after every window — a capture that leaked a nonzero rate would keep sampling for the life of the process.

## Contributing

Got an idea? Throw in a PR. Found a bug? Raise an issue.

## License

MIT. See [LICENSE](LICENSE).

See [CHANGELOG.md](CHANGELOG.md) for release notes.
