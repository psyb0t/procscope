# procscope

[![Go Reference](https://pkg.go.dev/badge/github.com/psyb0t/procscope.svg)](https://pkg.go.dev/github.com/psyb0t/procscope)
[![CI](https://github.com/psyb0t/procscope/actions/workflows/pipeline.yml/badge.svg?branch=main)](https://github.com/psyb0t/procscope/actions/workflows/pipeline.yml)
[![coverage](https://raw.githubusercontent.com/psyb0t/procscope/badges/coverage.svg)](https://github.com/psyb0t/procscope/actions/workflows/pipeline.yml)
[![version](https://raw.githubusercontent.com/psyb0t/procscope/badges/version.svg)](https://github.com/psyb0t/procscope/tags)
[![license](https://raw.githubusercontent.com/psyb0t/procscope/badges/license.svg)](LICENSE)

Go library that lets a process profile itself — what it's burning CPU on, what's holding memory, what every goroutine is doing — and returns the answer as a **value** instead of serving it on a port.

**Status:** active, early.

## Contents

- [Why Not Just /debug/pprof](#why-not-just-debugpprof)
- [Install](#install)
- [Quick Start](#quick-start)
- [Packages](#packages)
- [Searching the process's own logs](#searching-the-processs-own-logs)
- [What This Costs At Rest](#what-this-costs-at-rest)
- [Building Agent Tooling On Top](#building-agent-tooling-on-top)
- [Dev Workflow](#dev-workflow)
- [Contributing](#contributing)
- [License](#license)

## Why Not Just /debug/pprof

`net/http/pprof` needs a listener, and the moment you actually want it — a container behind a load balancer, mid-incident, no route to that port — the listener is the one thing you can't reach.

procscope returns profiles as plain structs, so whatever channel you *already* trust to reach the process can serve them: an authenticated admin route, an MCP tool, a CLI subcommand, an RPC.

It composes rather than replaces. Keep shipping logs and metrics somewhere durable; this is the fast path when you need an answer out of *this* process, *now*.

## Install

```bash
go get github.com/psyb0t/procscope
```

## Quick Start: profile the process

A complete program:

```go
package main

import (
	"fmt"

	"github.com/psyb0t/procscope/pprofcapture"
)

func main() {
	// kind=heap, debug=0 (protobuf), seconds=0 (heap is a snapshot, not a window)
	result, err := pprofcapture.Capture(pprofcapture.KindHeap, pprofcapture.DebugProto, 0)
	if err != nil {
		panic(err)
	}

	fmt.Println(result.Encoding) // base64+gzip
	fmt.Println(result.Profile)  // base64 of a gzipped pprof protobuf
}
```

Feed it to the real tooling:

```bash
go run . | tail -1 | base64 -d > prof.pb.gz && go tool pprof -http=: prof.pb.gz
```

Eight profile kinds, and block/mutex profiling that arms itself for a bounded window and restores the process-global knobs afterwards → **[pprofcapture/README.md](pprofcapture/README.md)**

## Packages

| Package | What it does |
|---|---|
| **[pprofcapture](pprofcapture/README.md)** | Live pprof profiles — 8 kinds, protobuf or text, self-arming block/mutex windows |

Dependencies are deliberately short: `common-go` for error sentinels and `ctxerrors` for wrapping. No Prometheus client, no MCP SDK, no `google/pprof` — whatever you build on top picks those.

## Searching the process's own logs

procscope does not do this, and deliberately so — [slog-configurator](https://github.com/psyb0t/slog-configurator) already does, on the ring handler itself:

```go
ring := logring.New(logring.Options{})
slogconfigurator.AddHandler(ring)   // the ring IS the slog.Handler

entries := ring.Search(logring.SearchOptions{
    Attrs:    map[string]string{"request_id": "abc123"},
    MinLevel: slog.LevelWarn,
    Limit:    50,
})
```

`Search`, `Count`, `Tail`, `Clear` and `Stats` are all methods on that handler, so there is nothing for procscope to wrap. `Attrs` matches structured attributes captured off the record, which is why it behaves the same in text or JSON mode and finds attributes bound upstream through `With` that never appear on the record itself.

A `logsearch` package shipped in v0.1.0 wrapping exactly that. It was removed in v0.2.0 — see [CHANGELOG.md](CHANGELOG.md).

## What This Costs At Rest

Nothing is enabled until you call it. No listener, no background goroutine, no sampler. Snapshot kinds read state the runtime already maintains, and block and mutex profiling stay off until a window asks for them and go back off afterwards.

## Building Agent Tooling On Top

Every result is a plain struct with `json` tags already shaped for a tool payload — which is why there's no MCP dependency here. You wire the transport; procscope supplies the answer.

The part worth getting right: **write the tool descriptions so a model can route between them.** Handed several equivalent-looking debug tools, a model picks badly. Say which one is the dashboard and which is the microscope, say what blocks and for how long, and say what each tool *cannot* see — a per-process ring that dies with the process has to advertise that, or the model will confidently report "no errors" for a replica it never looked at.

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
